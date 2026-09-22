package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
)

func newGC(a *app) *cobra.Command {
	var (
		planOnly  bool
		in        apps.GCInput
		keepCache string
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove containers, images and build cache that no kept revision uses.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if keepCache != "" {
				n, err := apps.ParseSize(keepCache)
				if err != nil {
					return fmt.Errorf("--keep-cache: %w", err)
				}
				in.BuildCacheMax = n
			}
			return a.operate(cmd.Context(), apps.GCKind, in, planOnly)
		},
	}
	cmd.Flags().BoolVar(&in.KeepBuildCache, "keep-build-cache", false, "leave Docker's build cache alone")
	cmd.Flags().StringVar(&keepCache, "keep-cache", "", "how much build cache to keep, such as 4g or 512m (default 8g)")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newExec(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "exec <app> [workload] -- <command...>",
		Short: "Run a command inside a workload's container.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 1 {
				return fmt.Errorf("give the app, optionally the workload, then -- and the command")
			}
			target, command := args[:dash], args[dash:]
			if len(command) == 0 {
				return fmt.Errorf("nothing to run after --")
			}
			container, err := a.resolveContainer(cmd.Context(), target[0], optional(target, 1))
			if err != nil {
				return err
			}
			dockerBin, err := exec.LookPath("docker")
			if err != nil {
				return err
			}
			argv := []string{"docker", "exec", "-i"}
			if a.tty {
				argv = append(argv, "-t")
			}
			argv = append(append(argv, container), command...)
			return a.runDataCommand(cmd.Context(), target[0], dockerBin, argv[1:], os.Environ())
		},
	}
}

// Keep the shared backup lock alive for the full operator command.
func (a *app) runDataCommand(ctx context.Context, appName, binary string, args, env []string) error {
	unlock, err := apps.AcquireDataAccess(ctx, filepath.Join(a.stateDir, "state.db"), appName)
	if err != nil {
		return err
	}
	defer unlock()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, a.stdout, a.stderr, env
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return quietError{code: exit.ExitCode()}
	}
	return err
}
