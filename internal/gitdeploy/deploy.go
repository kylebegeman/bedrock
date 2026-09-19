package gitdeploy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/api"
	"github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/state"
	"github.com/kylebegeman/quark/internal/ui"
)

// Deployer runs a deploy and reports its events.
type Deployer func(ctx context.Context, in app.DeployInput, emit func(kernel.Event)) (*kernel.Receipt, error)

// ThroughDaemon asks the daemon on a socket to deploy. It never falls back
// to a kernel of its own: the quark user can't touch the machine's state,
// and shouldn't.
func ThroughDaemon(socket string) Deployer {
	return func(ctx context.Context, in app.DeployInput, emit func(kernel.Event)) (*kernel.Receipt, error) {
		c := api.Dial(socket)
		defer c.Close()
		if !c.Reachable(ctx) {
			return nil, fmt.Errorf("the quark daemon isn't answering on %s", socket)
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		return c.Run(ctx, app.DeployKind, raw, emit)
	}
}

// Deploy deploys a source tree, writing its steps to out the way the CLI
// shows them without a terminal. It reports whether the deploy succeeded.
func Deploy(ctx context.Context, d Deployer, source, commit string, out io.Writer) (bool, error) {
	in := app.DeployInput{Source: source, Revision: time.Now().UTC().Format("20060102-150405"), Commit: commit}
	r := ui.New(out, false, false)
	receipt, err := d(ctx, in, r.Event)
	if err != nil {
		return false, err
	}
	return receipt.Status == state.Succeeded, nil
}

const zeroCommit = "0000000000000000000000000000000000000000"

// hookBuildsDir is where the hook writes source trees; tests move it.
var hookBuildsDir = BuildsDir

// Hook is the pre-receive hook: a push to the deploy branch deploys that
// commit, and a failed deploy refuses the push, so the branch on the
// machine is always what runs. Other branches are kept and not deployed.
func Hook(ctx context.Context, appName string, stdin io.Reader, out io.Writer, d Deployer) int {
	repo, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	branch := Branch(repo)
	target := ""
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		if f[2] != "refs/heads/"+branch {
			fmt.Fprintf(out, "quark: %s kept; pushes to %s deploy\n", strings.TrimPrefix(f[2], "refs/heads/"), branch)
			continue
		}
		if f[1] == zeroCommit {
			fmt.Fprintf(out, "quark: %s is the branch that deploys %s; it can't be deleted here\n", branch, appName)
			return 1
		}
		target = f[1]
	}
	if target == "" {
		return 0
	}
	short := target[:12]
	fmt.Fprintf(out, "quark: deploying %s of %s\n", short, appName)
	dir, err := NewBuildDir(hookBuildsDir, appName, short)
	if err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	defer PruneBuilds(hookBuildsDir, appName, 3)
	if err := ExportCommit(ctx, repo, target, dir); err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	return deployTree(ctx, appName, dir, short, out, d, "the push is refused and the running revision stays")
}

// ReceiveTree takes a source tree as a tar stream, as quark deploy --to
// sends it, and deploys it.
func ReceiveTree(ctx context.Context, appName string, stdin io.Reader, out io.Writer, d Deployer, buildsRoot string) int {
	dir, err := NewBuildDir(buildsRoot, appName, "upload")
	if err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	defer PruneBuilds(buildsRoot, appName, 3)
	if err := Extract(stdin, dir); err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "quark: deploying %s from the uploaded tree\n", appName)
	return deployTree(ctx, appName, dir, "", out, d, "the running revision stays")
}

func deployTree(ctx context.Context, appName, dir, commit string, out io.Writer, d Deployer, onFailure string) int {
	m, err := CheckSource(dir, appName)
	if err != nil {
		fmt.Fprintf(out, "quark: %v\n", err)
		return 1
	}
	ok, err := Deploy(ctx, d, dir, commit, out)
	if errors.Is(err, api.ErrDisconnected) {
		fmt.Fprintf(out, "quark: %v; the daemon finishes the deploy on its own\n", err)
		return 1
	}
	if err != nil {
		fmt.Fprintf(out, "quark: %v; %s\n", err, onFailure)
		return 1
	}
	if !ok {
		fmt.Fprintf(out, "quark: the deploy failed; %s\n", onFailure)
		return 1
	}
	fmt.Fprintf(out, "quark: %s is live%s\n", appName, liveAt(m))
	return 0
}

func liveAt(m *manifest.Manifest) string {
	hosts := m.Hosts()
	if len(hosts) == 0 {
		return ""
	}
	return " at https://" + hosts[0]
}
