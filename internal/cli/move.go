package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/host"
	"github.com/kylebegeman/bedrock/internal/integration"
)

func newMove(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move",
		Short: "Move an app, and its data, to another machine.",
		Long: `Move an app, and its data, to another machine.

A move is two commands, one on each machine, with the backup bucket as the
channel between them. Both machines must be pointed at the same storage: the
same bucket prefix and the same backup password. A machine reading its own
bucket instead of the source's finds nothing there, which looks like an app
with no data rather than like a failure.

  on the source:  bedrock move out <app> > handoff.json
  on the target:  bedrock move in <app> --handoff handoff.json
                  bedrock deploy <source>
                  bedrock dns point <host>          # once it answers
  on the source:  bedrock remove <app>              # after a rollback window

The source keeps running and keeps serving throughout. Nothing is torn down
until you tear it down, which is what makes the window a real one.`,
	}
	cmd.AddCommand(newMoveOut(a), newMoveIn(a))
	return cmd
}

func newMoveOut(a *app) *cobra.Command {
	var skipBackup bool
	cmd := &cobra.Command{
		Use:   "out <app>",
		Short: "Back an app up and write the handoff the target machine reads.",
		Long: "Back an app up and write the handoff the target machine reads.\n\n" +
			"The handoff goes to standard output and carries no secrets, so it can be\n" +
			"redirected to a file, read, and copied to the other machine.\n\n" +
			"The app is left running. A move is only safe because the source is still\n" +
			"there to go back to.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !skipBackup {
				// The snapshot named in the handoff has to be the last one,
				// or the target restores data the source has since moved on
				// from. Taking it here is what makes that true.
				a.yes = true
				if err := a.operate(cmd.Context(), apps.BackupKind, apps.BackupInput{App: name}, false); err != nil {
					return err
				}
			}
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			h, err := apps.NewHandoff(cmd.Context(), store, a.secretsStore(), name, thisMachine(cmd.Context()))
			if err != nil {
				return err
			}
			if !a.json {
				for _, line := range h.Describe() {
					fmt.Fprintln(a.stderr, "  "+line)
				}
				fmt.Fprintln(a.stderr)
			}
			return apps.WriteHandoff(a.stdout, h)
		},
	}
	cmd.Flags().BoolVar(&skipBackup, "no-backup", false, "use the last good backup instead of taking a fresh one")
	return cmd
}

func newMoveIn(a *app) *cobra.Command {
	var (
		handoffPath string
		planOnly    bool
	)
	cmd := &cobra.Command{
		Use:   "in <app>",
		Short: "Restore an app's data here from the handoff the source machine wrote.",
		Long: "Restore an app's data here from the handoff the source machine wrote.\n\n" +
			"This brings the data across and stops. Deploying the app is a separate\n" +
			"step, so the restore can be checked before anything starts serving, and\n" +
			"so the source keeps answering until you move DNS yourself.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var in io.Reader = os.Stdin
			if handoffPath != "" && handoffPath != "-" {
				f, err := os.Open(handoffPath)
				if err != nil {
					return err
				}
				defer f.Close()
				in = f
			}
			h, err := apps.ReadHandoff(in)
			if err != nil {
				return err
			}
			st, stErr := integration.LoadStorage(a.secretsStore())
			if stErr != nil {
				st = nil
			}
			if err := h.Check(name, st); err != nil {
				return err
			}
			for _, line := range h.Describe() {
				fmt.Fprintln(a.stderr, "  "+line)
			}
			fmt.Fprintln(a.stderr)
			if !h.HasData {
				fmt.Fprintf(a.stdout, "%s keeps no data; deploy it here and move its DNS.\n", name)
				return nil
			}
			if err := a.operate(cmd.Context(), apps.RestoreKind, apps.RestoreInput{App: name, Snapshot: h.Snapshot}, planOnly); err != nil {
				return err
			}
			if planOnly {
				return nil
			}
			fmt.Fprintf(a.stdout, "\n%s's data is here. Next: deploy it, check it answers, then point %s at this machine.\n",
				name, hostsOrIts(h.Hosts))
			return nil
		},
	}
	cmd.Flags().StringVar(&handoffPath, "handoff", "-", "the handoff written by 'bedrock move out', or - for standard input")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func hostsOrIts(hosts []string) string {
	if len(hosts) == 0 {
		return "its hostnames"
	}
	out := hosts[0]
	for _, h := range hosts[1:] {
		out += ", " + h
	}
	return out
}

// thisMachine names the source in a handoff. It is for the record only, so
// a machine that cannot say its own name does not stop a move.
func thisMachine(ctx context.Context) string {
	name, _ := host.RealEnv().Run(ctx, "hostname")
	return strings.TrimSpace(name)
}
