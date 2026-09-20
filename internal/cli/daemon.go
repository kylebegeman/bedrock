package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/daemon"
)

func newDaemon(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "The host daemon: run it, install it under systemd, or check it.",
	}
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (what systemd starts).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return daemon.Run(cmd.Context(), daemon.Config{StateDir: a.stateDir, Socket: a.socket}, a.stderr)
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install this binary as the systemd service and start it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return daemon.Install(cmd.Context(), a.stdout)
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Report the service's state and whether the daemon answers.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			text, err := daemon.Status(cmd.Context(), a.socket)
			fmt.Fprintln(a.stdout, text)
			return err
		},
	}
	cmd.AddCommand(run, install, status)
	return cmd
}
