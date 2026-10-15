package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/helios/internal/app"
	"github.com/kylebegeman/helios/internal/manifest"
)

func newMoveCapture(a *app, final bool) *cobra.Command {
	verb, short := "rehearse", "Capture a rehearsal snapshot while the source remains available afterwards."
	if final {
		verb, short = "final", "Fence all source writers and capture the final handoff; the source stays stopped."
	}
	var target string
	var planOnly bool
	cmd := &cobra.Command{Use: verb + " <app>", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if target == "" {
			return fmt.Errorf("give --to with the target's machine name (hostname on that machine)")
		}
		store, err := a.openState()
		if err != nil {
			return err
		}
		defer store.Close()
		if !planOnly {
			a.narrate = a.stderr
		}
		input, err := apps.MoveCaptureInputFor(cmd.Context(), store, args[0], thisMachine(), target, final)
		if err != nil {
			return err
		}
		if err := a.operate(cmd.Context(), apps.MoveCaptureKind, input, planOnly); err != nil {
			return err
		}
		if planOnly {
			return nil
		}
		phase := "rehearsal"
		if final {
			phase = "final"
		}
		h, err := apps.ReadMoveHandoff(store.Path(), args[0], phase)
		if err != nil {
			return err
		}
		return apps.WriteHandoff(a.stdout, h)
	}}
	cmd.Flags().StringVar(&target, "to", "", "the exact target machine name, as hostname reports there")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func readMoveInput(path string) (*apps.Handoff, error) {
	var input io.Reader = os.Stdin
	if path != "" && path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		input = f
	}
	return apps.ReadHandoff(input)
}

func newMoveStage(a *app) *cobra.Command {
	var handoff, manifestName, sourceCommit string
	var planOnly bool
	cmd := &cobra.Command{Use: "stage <source-dir>", Short: "Restore the final snapshot and validate the target without public routes or DNS.", Long: "Restore the final snapshot and validate the target without public routes or DNS.\n\nImport the source secrets first with move secrets. The handoff must come from\nmove final and name this target machine. Staging starts web and static\nworkloads for direct checks; worker, cron and release jobs wait. Staged\nworkloads may write destination data, so resuming the source never recovers\nthose changes. Use move cutover to admit normal destination writers and DNS.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		h, err := readMoveInput(handoff)
		if err != nil {
			return err
		}
		source, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		input := apps.MoveStageInput{Source: source, Manifest: manifestName, Revision: apps.MoveTargetRevision(*h), Commit: sourceCommit, Handoff: *h}
		return a.operate(cmd.Context(), apps.MoveStageKind, input, planOnly)
	}}
	cmd.Flags().StringVar(&handoff, "handoff", "-", "the final source handoff, or - for standard input")
	cmd.Flags().StringVar(&manifestName, "manifest", manifest.FileName, "the source manifest")
	cmd.Flags().StringVar(&sourceCommit, "source-commit", "", "preserved source commit for an uploaded archive")
	_ = cmd.Flags().MarkHidden("source-commit")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newMoveCutover(a *app) *cobra.Command {
	var planOnly bool
	cmd := &cobra.Command{Use: "cutover <app>", Short: "Recheck the staged target, admit its writers and move production routes and DNS.", Long: "Recheck the staged target, admit its writers and move production routes and DNS.\n\nThis is the explicit write-authority boundary. Once destination writers\nstart, source data may be stale. A failure leaves the source fenced and the\ntarget intact so the operation can be repaired and retried. Helios never\nautomatically points production back at a stale source.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := a.openState()
		if err != nil {
			return err
		}
		defer store.Close()
		input, err := apps.MigrationCutoverInput(store.Path(), args[0])
		if err != nil {
			return err
		}
		return a.operate(cmd.Context(), apps.MoveCutoverKind, input, planOnly)
	}}
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newMoveResume(a *app) *cobra.Command {
	var targetStopped, planOnly bool
	cmd := &cobra.Command{Use: "resume <app>", Short: "Deliberately resume a frozen source after stopping the target.", Long: "Deliberately resume a frozen source after stopping the target.\n\n--target-stopped records your assertion that no destination writers remain.\nThe source cannot prove this across disconnected machines. Its data excludes\nchanges made on the destination; use a reverse move to preserve those writes.\nResume does not change DNS. Review DNS separately before returning traffic.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		store, err := a.openState()
		if err != nil {
			return err
		}
		defer store.Close()
		input, err := apps.MigrationResumeInput(store.Path(), args[0], targetStopped)
		if err != nil {
			return err
		}
		return a.operate(cmd.Context(), apps.MoveResumeKind, input, planOnly)
	}}
	cmd.Flags().BoolVar(&targetStopped, "target-stopped", false, "confirm that destination writers have been stopped; source data excludes destination writes")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newMoveTest(a *app) *cobra.Command {
	cmd := newMoveStage(a)
	cmd.Use = "test <source-dir>"
	cmd.Short = "Prove a rehearsal snapshot on scratch target data without production activation."
	cmd.Long = "Prove a rehearsal snapshot on scratch target data without production activation.\n\nImport original secrets first. Use the handoff from move rehearse. The test\nrestores scratch volumes, starts web and static workloads on an internal\nnetwork, checks them directly, and cleans up. Background workers, cron,\nrelease jobs, public routes and DNS are excluded. The source continues\nrunning after its capture; perform move final before a real cutover."
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("handoff")
		h, err := readMoveInput(path)
		if err != nil {
			return err
		}
		source, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		name, _ := cmd.Flags().GetString("manifest")
		commit, _ := cmd.Flags().GetString("source-commit")
		planOnly, _ := cmd.Flags().GetBool("plan")
		input := apps.MoveStageInput{Source: source, Manifest: name, Commit: commit, Revision: "test-" + apps.MoveTargetRevision(*h), Handoff: *h}
		return a.operate(cmd.Context(), apps.MoveTestKind, input, planOnly)
	}
	return cmd
}
