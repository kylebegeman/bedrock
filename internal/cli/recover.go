package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
)

func newRecover(a *app) *cobra.Command {
	var in apps.RecoverMachineInput
	var identityFile string
	var identityStdin, planOnly bool
	cmd := &cobra.Command{
		Use:   "recover <source-machine>",
		Short: "Recover an exact machine backup privately, or rehearse it in disposable isolation.",
		Long: `Recover an exact machine backup privately, or rehearse it in disposable isolation.

Configure the target's own identity and storage integration first. The source
recovery identity is read from a protected file or stdin, then sealed with the
target identity for resumable operation. It never appears in operation input.

Recovery restores recorded images, sealed secret versions, pinned app data,
host profile and operational history. It starts workloads privately with no
published ports or live routes. A separate cutover changes live traffic after
readiness. --rehearsal skips target state/profile/integrations and removes its
isolated workloads and volumes after the check.

  bedrock recover old-host --snapshot <id> --identity-file ./recovery.key --plan
  bedrock recover old-host --snapshot <id> --identity-file ./recovery.key --yes
  bedrock recover old-host --snapshot <id> --identity-stdin --rehearsal --yes
  bedrock recover cutover <recovery-id> --plan

The machine snapshot must contain a compatible recovery catalog and workload
image archive. Data recovery restores the app recovery points embedded inside
that machine snapshot, so app retention cannot remove them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if identityFile != "" && identityStdin {
				return errors.New("choose either --identity-file or --identity-stdin")
			}
			in.Source = args[0]
			if in.ID == "" {
				in.ID = apps.RecoveryID(in.Source, in.Snapshot, in.Rehearsal)
			}
			if err := apps.ValidateMachineRecoveryInput(in); err != nil {
				return err
			}
			if !planOnly {
				var input io.Reader
				var file *os.File
				if identityFile != "" {
					var err error
					file, err = openRecoveryIdentityFile(identityFile)
					if err != nil {
						return err
					}
					defer file.Close()
					input = file
				} else if identityStdin {
					if !a.yes && a.digest == "" {
						return errors.New("--identity-stdin needs --yes or an approved --digest because stdin is reserved for the identity")
					}
					input = cmd.InOrStdin()
				}
				var err error
				if input != nil {
					in.IdentitySHA256, err = apps.SealRecoveryIdentity(a.secretsStore(), a.stateDir, in.ID, input)
				} else {
					in.IdentitySHA256, err = apps.RecoveryIdentityHash(a.stateDir, in.ID)
					if err != nil {
						return errors.New("supply the source recovery identity through --identity-file or --identity-stdin for a new recovery")
					}
				}
				if err != nil {
					return err
				}
			}
			return a.operate(cmd.Context(), apps.RecoverMachineKind, in, planOnly)
		},
	}
	cmd.Flags().StringVar(&in.Snapshot, "snapshot", "", "explicit machine snapshot ID (required; latest is not accepted)")
	cmd.Flags().StringVar(&in.ID, "id", "", "stable recovery ID; defaults to a hash of source, snapshot and mode")
	cmd.Flags().BoolVar(&in.Rehearsal, "rehearsal", false, "prove recovery in disposable internal networks without changing target state, profile or traffic")
	cmd.Flags().StringVar(&identityFile, "identity-file", "", "protected source recovery identity file; never passed to the operation")
	cmd.Flags().BoolVar(&identityStdin, "identity-stdin", false, "read source recovery identity from stdin; requires --yes or --digest")
	a.mutatingFlags(cmd, &planOnly)
	_ = cmd.MarkFlagRequired("snapshot")
	var cutoverPlan bool
	cutover := &cobra.Command{Use: "cutover <recovery-id>", Short: "Route live traffic to a completed private machine recovery after readiness.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.operate(cmd.Context(), apps.RecoverCutoverKind, apps.RecoverCutoverInput{ID: args[0]}, cutoverPlan)
	}}
	a.mutatingFlags(cutover, &cutoverPlan)
	report := &cobra.Command{Use: "report <recovery-id>", Short: "Show the recovery's pinned snapshots, private readiness and cutover status.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		report, err := apps.ReadMachineRecoveryReport(a.stateDir, args[0])
		if err != nil {
			return err
		}
		if a.json {
			return json.NewEncoder(a.stdout).Encode(report)
		}
		fmt.Fprintf(a.stdout, "%s: %s\nsource %s, machine snapshot %s\n", report.ID, report.Status, report.Source, report.Snapshot)
		for _, app := range sortedKeys(report.PrivateReady) {
			snapshot := report.AppSnapshots[app]
			if snapshot == "" {
				snapshot = "no data"
			}
			fmt.Fprintf(a.stdout, "%s: data snapshot %s, private ready %t\n", app, snapshot, report.PrivateReady[app])
		}
		return nil
	}}
	var abandonPlan bool
	abandon := &cobra.Command{Use: "abandon <recovery-id>", Short: "Stop private recovery containers and release its reservation, preserving restored data and history.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.operate(cmd.Context(), apps.RecoverAbandonKind, apps.RecoverCutoverInput{ID: args[0]}, abandonPlan)
	}}
	a.mutatingFlags(abandon, &abandonPlan)
	cmd.AddCommand(cutover, report, abandon)
	return cmd
}
