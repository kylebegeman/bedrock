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

// launching is one run of launch: its flags, and the steps from a
// repository to a running app.
type launching struct {
	a        *app
	appName  string
	host     string
	branch   string
	postgres bool
	port     int
	planOnly bool
	keep     bool
}

func newLaunch(a *app) *cobra.Command {
	l := &launching{a: a}
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
			return l.run(cmd.Context(), args[0])
		},
	}
	cmd.Flags().StringVar(&l.appName, "app", "", "the app's name (default: the repository's name)")
	cmd.Flags().StringVar(&l.host, "host", "", "the hostname it answers on")
	cmd.Flags().StringVar(&l.branch, "branch", "main", "the branch to launch")
	cmd.Flags().BoolVar(&l.postgres, "postgres", false, "give the app a database, and DATABASE_URL to reach it")
	cmd.Flags().IntVar(&l.port, "port", 0, fmt.Sprintf("the port a web app listens on (default %d)", manifest.DefaultPort))
	cmd.Flags().BoolVar(&l.keep, "keep", false, "leave the fetched tree and the manifest bedrock wrote behind after a --plan or a failure, to read them")
	a.mutatingFlags(cmd, &l.planOnly)
	return cmd
}

// check refuses, before anything is fetched, what can be refused then.
func (l *launching) check(repo string) error {
	if err := gitdeploy.ValidRepo(repo); err != nil {
		return err
	}
	if err := gitdeploy.ValidBranch(l.branch); err != nil {
		return err
	}
	if l.appName == "" {
		l.appName = appNameFromRepo(repo)
	}
	if l.appName == "" {
		return fmt.Errorf("could not read an app name out of %q; pass --app", repo)
	}
	if err := (manifest.Scaffold{App: l.appName, Kind: manifest.Worker}).Check(); err != nil {
		return err
	}
	if l.host != "" && !manifest.ValidHost(l.host) {
		return fmt.Errorf("--host %q isn't a hostname such as %s.example.com", l.host, l.appName)
	}
	return nil
}

func (l *launching) run(ctx context.Context, repo string) error {
	a := l.a
	if err := l.check(repo); err != nil {
		return err
	}
	// The tree lives beside the other sources deploys are made from, and
	// goes when the launch does not: a planned or failed one leaves nothing
	// behind unless asked to, and a deployed one is kept, the newest few per
	// app, because its revision names it.
	root := filepath.Join(a.stateDir, "builds")
	dir, err := gitdeploy.NewBuildDir(root, l.appName, "launch")
	if err != nil {
		return err
	}
	deployed := false
	defer func() {
		switch {
		case deployed:
			gitdeploy.PruneBuilds(root, l.appName, 3)
		case l.keep:
			fmt.Fprintf(a.stderr, "the fetched tree and its manifest are at %s\n", dir)
		default:
			_ = os.RemoveAll(dir)
		}
	}()
	fmt.Fprintf(a.stderr, "fetching %s\n", repo)
	if err := shallowClone(ctx, repo, l.branch, dir); err != nil {
		return err
	}
	path, err := l.writeManifest(dir)
	if err != nil {
		return err
	}
	in := apps.DeployInput{Source: dir, Revision: apps.NewRevision(time.Now())}
	if err := a.operate(ctx, apps.DeployKind, in, l.planOnly); err != nil {
		return err
	}
	if l.planOnly {
		return nil
	}
	deployed = true
	fmt.Fprintf(a.prose(), "\n%s is live. Its manifest is at %s; copy it into the repository so the next deploy uses yours.\n", l.appName, path)
	return nil
}

// writeManifest works out what the source is, writes the manifest for it
// and says what is still to do. It returns the manifest's path.
func (l *launching) writeManifest(dir string) (string, error) {
	a := l.a
	found, err := apps.Detect(dir)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(a.stderr, "%s looks like a %s app (%s)\n", l.appName, found.Kind, found.Why)
	s := manifest.Scaffold{App: l.appName, Kind: found.Kind, Host: l.host, Port: l.port, Postgres: l.postgres, Dir: found.Dir, Existing: true}
	body, err := s.Render()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, manifest.FileName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	fmt.Fprintf(a.stderr, "wrote %s\n", manifest.FileName)
	if next := s.Next(); len(next) > 0 {
		fmt.Fprintln(a.stderr, "still to do:")
		for _, step := range next {
			fmt.Fprintf(a.stderr, "  - %s\n", step)
		}
	}
	fmt.Fprintln(a.stderr)
	return path, nil
}

// shallowClone brings one branch down, without history and without ever
// stopping to ask for a password: a launch that needs credentials should
// fail saying so rather than hang. The branch was checked by the caller;
// the -- keeps a repository from being read as a flag either way.
func shallowClone(ctx context.Context, repo, branch, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", branch, "--", repo, dir)
	clone.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s %s: %s", repo, branch, strings.TrimSpace(string(out)))
	}
	// The history is not part of the app, and leaving it makes the tree the
	// build context several times larger than the source.
	return os.RemoveAll(filepath.Join(dir, ".git"))
}

var repoName = regexp.MustCompile(`([a-zA-Z0-9][a-zA-Z0-9._-]*?)(\.git)?/?$`)

// appNameFromRepo reads the app's name out of a repository URL, as a
// manifest will accept it.
func appNameFromRepo(repo string) string {
	m := repoName.FindStringSubmatch(strings.TrimSpace(repo))
	if m == nil {
		return ""
	}
	return apps.DNSLabel(m[1])
}
