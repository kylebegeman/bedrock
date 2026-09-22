// Package cli builds the bedrock command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/api"
	"github.com/kylebegeman/bedrock/internal/daemon"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/kylebegeman/bedrock/internal/ui"
	"github.com/kylebegeman/bedrock/internal/version"
)

// Main runs the command line with the given arguments and returns the
// process exit code. Output goes to stdout; errors go to stderr, once.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot(stdout, stderr)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		var quiet quietError
		if errors.As(err, &quiet) {
			return quiet.code
		}
		fmt.Fprintln(stderr, "bedrock:", err)
		return 1
	}
	return 0
}

// app holds what every command shares.
type app struct {
	stdout, stderr io.Writer
	stateDir       string
	socket         string
	json           bool
	tty            bool
	yes            bool
	digest         string
}

func (a *app) renderer() *ui.Renderer { return ui.New(a.stdout, a.tty, a.json) }

// runner returns a daemon client when a daemon answers on the socket, and
// otherwise the local kernel over the state directory.
func (a *app) runner(ctx context.Context, owner string) (api.Runner, error) {
	if a.socket != "" {
		client := api.Dial(a.socket)
		if client.Reachable(ctx) {
			return client, nil
		}
		client.Close()
	}
	store, err := state.Open(filepath.Join(a.stateDir, "state.db"))
	if err != nil {
		return nil, err
	}
	return &api.Local{Engine: kernel.New(store, daemon.RegistryIn(store, a.secretsStore(), a.socket, a.stateDir), owner), Store: store}, nil
}

func newRoot(stdout, stderr io.Writer) *cobra.Command {
	a := &app{stdout: stdout, stderr: stderr}
	if f, ok := stdout.(*os.File); ok {
		a.tty = ui.IsTerminal(f)
	}
	root := &cobra.Command{
		Use:           "bedrock",
		Short:         "One small program that runs your machines.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&a.stateDir, "state-dir", envOr("BEDROCK_STATE_DIR", defaultStateDir()), "where bedrock keeps its state")
	root.PersistentFlags().StringVar(&a.socket, "socket", envOr("BEDROCK_SOCKET", daemon.DefaultSocket), "the daemon's socket; the local kernel is used when nothing answers")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON, one object per line")
	root.AddCommand(newVersion(a), newInit(a), newDoctor(a), newHost(a), newUpgrade(a), newDeploy(a), newRollback(a), newLs(a), newStatus(a), newExposure(a), newDNS(a), newPs(a), newLogs(a), newExec(a), newGC(a), newMove(a), newSecret(a), newIntegration(a), newRun(a), newJobs(a), newPsql(a), newBackup(a), newBackups(a), newDrill(a), newRestore(a), newAlerts(a), newWatch(a), newRemove(a), newGit(a), newReceive(a), newHistory(a), newKernel(a), newDaemon(a))
	return root
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// defaultStateDir is /var/lib/bedrock for root on Linux (the machines) and
// ~/.bedrock anywhere else (the Mac).
func defaultStateDir() string {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		return daemon.DefaultStateDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".bedrock"
	}
	return filepath.Join(home, ".bedrock")
}

func newVersion(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of this build.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			info := version.Current()
			if a.json {
				return info.WriteJSON(a.stdout)
			}
			_, err := fmt.Fprintln(a.stdout, info)
			return err
		},
	}
}
