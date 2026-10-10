package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/spf13/cobra"
)

func newRehearse(a *app) *cobra.Command {
	var planOnly bool
	var snapshot string
	cmd := &cobra.Command{Use: "rehearse <source>", Short: "Test a candidate's releases and checks on an isolated restored backup.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		source, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		return a.operate(cmd.Context(), apps.RehearseKind, apps.RehearseInput{Source: source, Snapshot: snapshot, Revision: apps.NewRevision(time.Now())}, planOnly)
	}}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "the snapshot to test; default latest, exact identity recorded in the proof")
	a.mutatingFlags(cmd, &planOnly)
	report := &cobra.Command{Use: "report <app> <candidate-revision>", Short: "Show the candidate's exact source, images, snapshot and rehearsal outcome.", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		proof, err := apps.ReadRehearsalProof(a.stateDir, args[0], args[1])
		if err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(a.stdout).Encode(proof)
		}
		fmt.Fprintf(a.stdout, "%s %s: %s\nactive revision %s\nsource sha256 %s\nmanifest sha256 %s\nsnapshot %s\n", proof.App, proof.Revision, proof.Status, proof.ActiveRevision, proof.SourceSHA256, proof.ManifestSHA256, proof.Snapshot)
		for _, name := range sortedKeys(proof.Images) {
			fmt.Fprintf(a.stdout, "%s: %s\n", name, proof.Images[name])
		}
		if proof.Error != "" {
			fmt.Fprintf(a.stdout, "error: %s\n", proof.Error)
		}
		return nil
	}}
	cmd.AddCommand(report)
	return cmd
}
