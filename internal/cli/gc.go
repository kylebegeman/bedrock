package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/quark/internal/app"
)

func newGC(a *app) *cobra.Command {
	var (
		planOnly bool
		in       apps.GCInput
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove containers, images and build cache that no kept revision uses.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.operate(cmd.Context(), apps.GCKind, in, planOnly)
		},
	}
	cmd.Flags().BoolVar(&in.KeepBuildCache, "keep-build-cache", false, "leave Docker's build cache alone")
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
			// Hand the terminal to docker exec entirely.
			return syscall.Exec(dockerBin, argv, os.Environ())
		},
	}
}
