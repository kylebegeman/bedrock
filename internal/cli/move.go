package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
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

  on the source:  bedrock move out <app> --yes > handoff.json
  on the target:  bedrock move in <app> --handoff handoff.json
                  bedrock deploy <source>
                  bedrock dns point <host>          # once it answers
  on the source:  bedrock remove <app>              # after a rollback window

The source keeps running and keeps serving throughout. Nothing is torn down
until you tear it down, which is what makes the window a real one.`,
	}
	cmd.AddCommand(newMoveOut(a), newMoveIn(a), newMoveSecrets(a))
	return cmd
}

func newMoveOut(a *app) *cobra.Command {
	var (
		skipBackup bool
		planOnly   bool
	)
	cmd := &cobra.Command{
		Use:   "out <app>",
		Short: "Back an app up and write the handoff the target machine reads.",
		Long: "Back an app up and write the handoff the target machine reads.\n\n" +
			"The backup is a maintenance window, so it is planned, asked about and\n" +
			"applied like any other operation; --digest names its plan. The handoff\n" +
			"goes to standard output alone and carries no secrets, so it can be\n" +
			"redirected to a file, read, and copied to the other machine.\n\n" +
			"The app is left running. A move is only safe because the source is still\n" +
			"there to go back to.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !planOnly {
				// Standard output is the handoff's; the backup's plan, the
				// question and its steps go beside it.
				a.narrate = a.stderr
			}
			if !skipBackup {
				// The snapshot named in the handoff has to be the last one,
				// or the target restores data the source has since moved on
				// from. Taking it here is what makes that true.
				if err := a.operate(cmd.Context(), apps.BackupKind, apps.BackupInput{App: name}, planOnly); err != nil {
					return err
				}
			}
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			h, err := apps.NewHandoff(cmd.Context(), store, a.secretsStore(), name, thisMachine())
			if planOnly {
				switch {
				case errors.Is(err, apps.ErrNoSnapshot) && !skipBackup:
					fmt.Fprintf(a.stderr, "then a handoff for %s naming the snapshot that backup makes\n", name)
					return nil
				case err != nil:
					return err
				}
				fmt.Fprintln(a.stderr, "then a handoff, which as it stands would say:")
				for _, line := range h.Describe() {
					fmt.Fprintln(a.stderr, "  "+line)
				}
				return nil
			}
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
	a.mutatingFlags(cmd, &planOnly)
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
			// A storage integration that is not set is one Check explains;
			// one that can't be read is a fault of its own.
			st, err := integration.LoadStorage(a.secretsStore())
			if err != nil && !errors.Is(err, integration.ErrNotSet) {
				return err
			}
			if err := h.Check(name, st); err != nil {
				return err
			}
			for _, line := range h.Describe() {
				fmt.Fprintln(a.stderr, "  "+line)
			}
			fmt.Fprintln(a.stderr)
			if !h.HasData {
				fmt.Fprintf(a.prose(), "%s keeps no data; deploy it here and move its DNS.\n", name)
				return nil
			}
			if err := a.operate(cmd.Context(), apps.RestoreKind, apps.RestoreInput{App: name, Snapshot: h.Snapshot}, planOnly); err != nil {
				return err
			}
			if planOnly {
				return nil
			}
			fmt.Fprintf(a.prose(), "\n%s's data is here. Next: deploy it, check it answers, then point %s at this machine.\n",
				name, hostsOrIts(h.Hosts))
			return nil
		},
	}
	cmd.Flags().StringVar(&handoffPath, "handoff", "-", "the handoff written by 'bedrock move out', or - for standard input")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

// hostsOrIts names an app's hostnames, or stands in for them when it has
// none to name.
func hostsOrIts(hosts []string) string {
	if len(hosts) == 0 {
		return "its hostnames"
	}
	return strings.Join(hosts, ", ")
}

// thisMachine names the source in a handoff. It is for the record only, so
// a machine that cannot say its own name does not stop a move.
func thisMachine() string {
	name, _ := os.Hostname()
	return name
}

func newMoveSecrets(a *app) *cobra.Command {
	var (
		recipient string
		accept    bool
	)
	cmd := &cobra.Command{
		Use:   "secrets <app>",
		Short: "Seal an app's secrets for the target machine, and accept them there.",
		Long: `Seal an app's secrets for the target machine, and accept them there.

A moved app has to arrive with the secrets it left with. Its database comes
back from a snapshot still holding the role password the source generated, so
a target that generated its own would be handed a database it cannot open.
bedrock refuses that restore rather than performing it, which is why this
step exists.

  on the target:  bedrock secret recipient
  on the source:  bedrock move secrets <app> --to <age1...> > secrets.age
  on the target:  bedrock move secrets <app> --accept < secrets.age

Only ciphertext is ever printed, sealed to the target's own key, and it stops
being readable ten minutes after it is made. The values never appear in a
terminal, a log or a shell history on either machine.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			switch {
			case accept && recipient != "":
				return fmt.Errorf("--accept reads secrets here; --to seals them for elsewhere")
			case accept:
				stored, err := a.secretsStore().ImportBundle(name, os.Stdin)
				if err != nil {
					return err
				}
				if len(stored) == 0 {
					fmt.Fprintf(a.stdout, "%s already held every secret in the bundle.\n", name)
					return nil
				}
				fmt.Fprintf(a.stdout, "stored %d secret(s) for %s: %s\n", len(stored), name, strings.Join(stored, ", "))
				return nil
			case recipient == "":
				return fmt.Errorf("give --to with the target's recipient (bedrock secret recipient, run there), or --accept to read a bundle here")
			}
			sealed, err := a.secretsStore().ExportBundle(name, recipient)
			if err != nil {
				return err
			}
			_, err = io.WriteString(a.stdout, sealed)
			return err
		},
	}
	cmd.Flags().StringVar(&recipient, "to", "", "the target machine's recipient, from 'bedrock secret recipient' there")
	cmd.Flags().BoolVar(&accept, "accept", false, "read a sealed bundle from standard input and store it here")
	return cmd
}
