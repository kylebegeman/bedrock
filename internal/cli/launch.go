package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

func newLaunch(a *app) *cobra.Command {
	var (
		appName  string
		host     string
		branch   string
		postgres bool
		port     int
		planOnly bool
		keep     bool
	)
	cmd := &cobra.Command{
		Use:   "launch <repository>",
		Short: "Take a repository from nothing to running here.",
		Long: `Take a repository from nothing to running here.

Fetches the repository, works out what it is, writes the manifest it is
missing and deploys it. A repository that already has a bedrock.yaml is not
launched: deploy it instead, because overwriting a manifest somebody wrote
with a guess is worse than refusing.

  bedrock launch https://github.com/you/site --host site.example.com
  bedrock launch git@github.com:you/api.git --host api.example.com --postgres

What it can work out on its own is deliberately narrow, because a wrong
guess becomes a manifest someone has to debug:

  a Dockerfile            a web service built from it
  public/index.html       a static site from that directory
  index.html at the root  a static site from the root

Anything else is refused with a suggestion, rather than guessed at. The
manifest it writes is an ordinary one: read it, change it, commit it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if err := gitdeploy.ValidRepo(repo); err != nil {
				return err
			}
			if appName == "" {
				appName = appNameFromRepo(repo)
			}
			if appName == "" {
				return fmt.Errorf("could not read an app name out of %q; pass --app", repo)
			}
			dir := filepath.Join(a.stateDir, "launches", fmt.Sprintf("%s-%d", appName, time.Now().UnixMilli()))
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				return err
			}
			// A planned launch fetched the tree only to read it, so it goes
			// again afterwards. A real one keeps it: the deployed revision
			// records that directory as its source.
			if planOnly && !keep {
				defer os.RemoveAll(dir)
			}
			fmt.Fprintf(a.stderr, "fetching %s\n", repo)
			if err := shallowClone(cmd.Context(), repo, branch, dir); err != nil {
				return err
			}
			found, err := apps.Detect(dir)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "%s looks like a %s app (%s)\n", appName, found.Kind, found.Why)

			s := manifest.Scaffold{App: appName, Kind: found.Kind, Host: host, Port: port, Postgres: postgres}
			body, err := s.Render()
			if err != nil {
				return err
			}
			// The scaffold always writes public/; this tree may keep its
			// files somewhere else. Re-parse rather than trust the edit.
			if found.Kind == manifest.Static && found.Dir != "public" {
				body = []byte(strings.Replace(string(body), "    dir: public\n", "    dir: "+found.Dir+"\n", 1))
				if _, err := manifest.Parse(body); err != nil {
					return fmt.Errorf("pointing the site at %s produced a manifest that will not load: %w", found.Dir, err)
				}
			}
			path := filepath.Join(dir, manifest.FileName)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "wrote %s\n\n", manifest.FileName)

			in := apps.DeployInput{Source: dir, Revision: time.Now().UTC().Format("20060102-150405")}
			if err := a.operate(cmd.Context(), apps.DeployKind, in, planOnly); err != nil {
				return err
			}
			if planOnly {
				return nil
			}
			fmt.Fprintf(a.stdout, "\n%s is live. Its manifest is at %s; copy it into the repository so the next deploy uses yours.\n", appName, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "the app's name (default: the repository's name)")
	cmd.Flags().StringVar(&host, "host", "", "the hostname it answers on")
	cmd.Flags().StringVar(&branch, "branch", "main", "the branch to launch")
	cmd.Flags().BoolVar(&postgres, "postgres", false, "give the app a database, and DATABASE_URL to reach it")
	cmd.Flags().IntVar(&port, "port", 0, fmt.Sprintf("the port a web app listens on (default %d)", manifest.DefaultPort))
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the fetched tree behind even if the launch is only planned")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

// shallowClone brings one branch down, without history and without ever
// stopping to ask for a password: a launch that needs credentials should
// fail saying so rather than hang.
func shallowClone(ctx context.Context, repo, branch, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", branch, repo, dir)
	clone.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s %s: %s", repo, branch, strings.TrimSpace(string(out)))
	}
	// The history is not part of the app, and leaving it makes the tree the
	// build context several times larger than the source.
	return os.RemoveAll(filepath.Join(dir, ".git"))
}

var repoName = regexp.MustCompile(`([a-zA-Z0-9][a-zA-Z0-9._-]*?)(\.git)?/?$`)

// appNameFromRepo reads the app's name out of a repository URL, lowercased
// and with anything a manifest will not accept turned into a hyphen.
func appNameFromRepo(repo string) string {
	m := repoName.FindStringSubmatch(strings.TrimSpace(repo))
	if m == nil {
		return ""
	}
	name := strings.ToLower(m[1])
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
