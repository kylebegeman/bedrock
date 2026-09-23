package cli

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/host"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

func newGit(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Deploy by pushing: which keys may push which apps, remotes, and GitHub webhooks.",
	}
	cmd.AddCommand(newGitAllow(a), newGitDeny(a), newGitKeys(a), newGitRemote(a), newGitBranch(a), newGitWebhook(a), newGitWebhooks(a), newGitServe(a), newGitHook(a))
	return cmd
}

// newGitAllow is bedrock git allow.
func newGitAllow(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "allow <app> [public key]",
		Short: "Let an SSH key deploy an app with git push or bedrock deploy --to. The key comes from the argument or stdin.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName := args[0]
			if !gitdeploy.ValidApp(appName) {
				return fmt.Errorf("%q isn't an app's name", appName)
			}
			text := strings.Join(args[1:], " ")
			if text == "" {
				b, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
				if err != nil {
					return err
				}
				text = string(b)
			}
			key, err := gitdeploy.ParseKey(text)
			if err != nil {
				return err
			}
			kf, err := gitdeploy.ReadKeys(gitdeploy.AuthorizedKeys)
			if err != nil {
				return err
			}
			key, err = kf.Allow(key, appName)
			if err != nil {
				return err
			}
			if err := writeKeys(kf); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s may deploy %s\n", key.Fingerprint(), strings.Join(key.Apps, ", "))
			fmt.Fprintf(a.stdout, "from its machine: git remote add bedrock %s && git push bedrock main\n", remoteFor(cmd.Context(), appName))
			return nil
		},
	}
	return cmd
}

// newGitDeny is bedrock git deny.
func newGitDeny(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deny <app> <fingerprint or public key>",
		Short: "Stop a key from deploying an app; a key left with no apps is removed.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			kf, err := gitdeploy.ReadKeys(gitdeploy.AuthorizedKeys)
			if err != nil {
				return err
			}
			if !kf.Deny(args[0], strings.Join(args[1:], " ")) {
				return fmt.Errorf("no key named %s deploys %s", strings.Join(args[1:], " "), args[0])
			}
			if err := writeKeys(kf); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "that key no longer deploys %s\n", args[0])
			return nil
		},
	}
	return cmd
}

// newGitKeys is bedrock git keys.
func newGitKeys(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "List the keys that may push, and the apps each deploys.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			kf, err := gitdeploy.ReadKeys(gitdeploy.AuthorizedKeys)
			if err != nil {
				return err
			}
			if a.json {
				type row struct {
					Fingerprint string   `json:"fingerprint"`
					Comment     string   `json:"comment,omitempty"`
					Apps        []string `json:"apps"`
				}
				var rows []row
				for _, k := range kf.Keys {
					rows = append(rows, row{k.Fingerprint(), k.Comment, k.Apps})
				}
				return json.NewEncoder(a.stdout).Encode(rows)
			}
			if len(kf.Keys) == 0 {
				fmt.Fprintln(a.stdout, "no key may push yet; bedrock git allow <app> <public key> adds one")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tCOMMENT\tDEPLOYS")
			for _, k := range kf.Keys {
				fmt.Fprintf(w, "%s\t%s\t%s\n", k.Fingerprint(), orDash(k.Comment), strings.Join(k.Apps, ", "))
			}
			return w.Flush()
		},
	}
	return cmd
}

// newGitRemote is bedrock git remote.
func newGitRemote(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote <app>",
		Short: "Print the git remote that deploys an app.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(a.stdout, remoteFor(cmd.Context(), args[0]))
			return nil
		},
	}
	return cmd
}

// newGitBranch is bedrock git branch.
func newGitBranch(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch <app> <branch>",
		Short: "Choose the branch whose pushes deploy an app (main by default).",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			dir, err := gitdeploy.EnsureRepo(args[0])
			if err != nil {
				return err
			}
			if err := gitdeploy.SetBranch(dir, args[1]); err != nil {
				return err
			}
			if err := chownToPushUser(dir); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "pushes to %s deploy %s\n", args[1], args[0])
			return nil
		},
	}
	return cmd
}

// newGitWebhook is bedrock git webhook.
func newGitWebhook(a *app) *cobra.Command {
	var (
		repo, hookBranch string
		off              bool
	)
	cmd := &cobra.Command{
		Use:   "webhook <app>",
		Short: "Deploy an app when GitHub says its branch moved; prints what to paste into GitHub, once.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			appName := args[0]
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			if off {
				if err := gitdeploy.Disable(ctx, store, a.secretsStore(), appName); errors.Is(err, state.ErrNotFound) {
					return fmt.Errorf("%s has no webhook", appName)
				} else if err != nil {
					return err
				}
				if err := apps.ReloadEdge(ctx, store, a.secretsStore()); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "%s no longer deploys on webhooks; remove the webhook on GitHub too\n", appName)
				return nil
			}
			if repo == "" {
				return errors.New("give --repo, the repository GitHub pushes, such as git@github.com:you/app.git")
			}
			rev, err := store.RevisionWithStatus(ctx, appName, state.RevisionActive)
			if err != nil {
				return err
			}
			if rev == nil {
				return fmt.Errorf("deploy %s once first: its webhook arrives through one of its hosts", appName)
			}
			var m manifest.Manifest
			if err := json.Unmarshal(rev.Manifest, &m); err != nil {
				return err
			}
			hosts := m.Hosts()
			if len(hosts) == 0 {
				return fmt.Errorf("%s has no hosts for a webhook to arrive on", appName)
			}
			if err := a.ensureSecretsKey(a.secretsStore()); err != nil {
				return err
			}
			w, err := gitdeploy.Configure(ctx, store, a.secretsStore(), appName, repo, hookBranch, hosts[0])
			if err != nil {
				return err
			}
			if err := apps.ReloadEdge(ctx, store, a.secretsStore()); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "On GitHub, in the repository's Settings, add a webhook:\n  Payload URL   %s\n  Content type  application/json\n  Events        just the push event\n", w.URL)
			fmt.Fprintf(a.stderr, "  Secret        %s\n(shown once, here, and nowhere else)\n", w.Secret)
			if w.DeployKey != "" {
				fmt.Fprintf(a.stdout, "and, under Deploy keys, this key, read-only:\n  %s\n", w.DeployKey)
			}
			fmt.Fprintf(a.stdout, "pushes to %s of %s now deploy %s\n", hookBranch, repo, appName)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "the repository: git@github.com:you/app.git, https://..., or file:///path for a test")
	cmd.Flags().StringVar(&hookBranch, "branch", gitdeploy.DefaultBranch, "the branch whose pushes deploy")
	cmd.Flags().BoolVar(&off, "off", false, "stop deploying on webhooks and forget the secret and key")
	return cmd
}

// newGitWebhooks is bedrock git webhooks.
func newGitWebhooks(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhooks",
		Short: "List the webhooks and what their last delivery did.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			hooks, err := store.GitHooks(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(hooks)
			}
			if len(hooks) == 0 {
				fmt.Fprintln(a.stdout, "no webhooks")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tREPOSITORY\tBRANCH\tLAST")
			for _, h := range hooks {
				last := "nothing yet"
				if !h.LastAt.IsZero() {
					last = fmt.Sprintf("%s %s: %s", h.LastAt.Local().Format("2006-01-02 15:04"), h.LastCommit, h.LastResult)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", h.App, h.Repo, h.Branch, last)
			}
			return w.Flush()
		},
	}
	return cmd
}

// newGitServe is bedrock git serve, what a push key runs.
func newGitServe(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "serve <apps...>",
		Short:  "What a push key runs: git receive-pack, upload-pack or a tarball deploy, for its apps only.",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := gitdeploy.ParseCommand(os.Getenv("SSH_ORIGINAL_COMMAND"))
			if err != nil {
				return err
			}
			if err := req.Allowed(args); err != nil {
				return err
			}
			switch req.Kind {
			case gitdeploy.Receive:
				dir, err := gitdeploy.EnsureRepo(req.App)
				if err != nil {
					return err
				}
				return execGit("receive-pack", dir)
			case gitdeploy.Upload:
				dir := gitdeploy.RepoDir(req.App)
				if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
					return fmt.Errorf("nothing has been pushed to %s yet", req.App)
				}
				return execGit("upload-pack", dir)
			default:
				if code := gitdeploy.ReceiveTree(cmd.Context(), req.App, os.Stdin, a.stdout, gitdeploy.ThroughDaemon(a.socket), gitdeploy.BuildsDir); code != 0 {
					return quietError{code}
				}
				return nil
			}
		},
	}
	return cmd
}

// newGitHook is bedrock git hook, an app repository's pre-receive hook.
func newGitHook(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook <app>",
		Short:  "The pre-receive hook of an app's repository.",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := gitdeploy.Hook(cmd.Context(), args[0], os.Stdin, a.stdout, gitdeploy.ThroughDaemon(a.socket)); code != 0 {
				return quietError{code}
			}
			return nil
		},
	}
	return cmd
}

// newReceive deploys a source tree sent on stdin, for bedrock deploy --to
// over a root login; the bedrock user's keys reach the same code through
// git serve.
func newReceive(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "receive <app>",
		Short:  "Deploy the source tree on stdin (what bedrock deploy --to sends).",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !gitdeploy.ValidApp(args[0]) {
				return fmt.Errorf("%q isn't an app's name", args[0])
			}
			root := gitdeploy.BuildsDir
			if os.Geteuid() == 0 {
				root = filepath.Join(a.stateDir, "builds")
			}
			if code := gitdeploy.ReceiveTree(cmd.Context(), args[0], os.Stdin, a.stdout, gitdeploy.ThroughDaemon(a.socket), root); code != 0 {
				return quietError{code}
			}
			return nil
		},
	}
}

func execGit(sub, dir string) error {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	return syscall.Exec(gitPath, []string{"git", sub, dir}, os.Environ())
}

// writeKeys writes the key file and gives it to the push user.
func writeKeys(kf *gitdeploy.KeyFile) error {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return errors.New("changing who may push needs root")
	}
	if err := kf.Write(); err != nil {
		return err
	}
	if err := chownToPushUser(filepath.Dir(kf.Path)); err != nil {
		return err
	}
	return chownToPushUser(kf.Path)
}

// chownToPushUser gives a path, and everything under it, to the bedrock user.
func chownToPushUser(path string) error {
	u, err := user.Lookup(gitdeploy.User)
	if err != nil {
		return fmt.Errorf("the %s user doesn't exist; run bedrock host reconcile", gitdeploy.User)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// remoteFor names the git remote that deploys an app on this machine.
func remoteFor(ctx context.Context, appName string) string {
	addr := "<this machine>"
	for _, a := range host.Addresses(ctx, host.RealEnv()) {
		if !strings.Contains(a, ":") {
			addr = a
			break
		}
	}
	return fmt.Sprintf("%s@%s:%s.git", gitdeploy.User, addr, appName)
}

// deployTo sends a source tree to a machine over SSH and deploys it
// there, printing the machine's steps as they happen.
func deployTo(ctx context.Context, a *app, dir, appName, target string) error {
	files, err := sourceFiles(ctx, dir)
	if err != nil {
		return err
	}
	var size int64
	for _, f := range files {
		if info, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			size += info.Size()
		}
	}
	fmt.Fprintf(a.stderr, "sending %d files (%s) of %s to %s\n", len(files), bytesWord(size), appName, target)
	ssh := strings.Fields(envOr("BEDROCK_SSH", "ssh"))
	args := append(append([]string{}, ssh[1:]...), target, "bedrock", "receive", appName)
	cmd := exec.CommandContext(ctx, ssh[0], args...)
	cmd.Stdout, cmd.Stderr = a.stdout, a.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	writeErr := writeTree(stdin, dir, files)
	stdin.Close()
	waitErr := cmd.Wait()
	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			return quietError{exit.ExitCode()}
		}
		return waitErr
	}
	return writeErr
}

// sourceFiles lists what to send: in a git checkout, the tracked files and
// the untracked ones git doesn't ignore; elsewhere, everything but .git
// and node_modules.
func sourceFiles(ctx context.Context, dir string) ([]string, error) {
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output(); err == nil {
		var files []string
		seen := map[string]bool{}
		for _, f := range strings.Split(string(out), "\x00") {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
				files = append(files, f)
			}
		}
		sort.Strings(files)
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

// writeTree writes files as a gzipped tar stream.
func writeTree(w io.Writer, dir string, files []string) error {
	bw := bufio.NewWriterSize(w, 256<<10)
	gz := gzip.NewWriter(bw)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		path := filepath.Join(dir, f)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() {
			continue
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(f)
		h.Uname, h.Gname, h.Uid, h.Gid = "", "", 0, 0
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, file)
			file.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return bw.Flush()
}
