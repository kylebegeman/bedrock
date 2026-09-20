package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/host"
)

func newDoctor(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine and say what to fix. Changes nothing.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			facts := host.Gather(cmd.Context(), host.RealEnv(), a.socket)
			results := host.Diagnose(facts)
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Facts   host.Facts    `json:"facts"`
					Results []host.Result `json:"results"`
					Worst   host.Verdict  `json:"worst"`
				}{facts, results, host.Worst(results)})
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			for _, r := range results {
				verdict := string(r.Verdict)
				if r.Verdict != host.Pass {
					verdict = strings.ToUpper(verdict)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, verdict, r.Detail, r.Fix)
			}
			w.Flush()
			switch host.Worst(results) {
			case host.Fail:
				return errors.New("this machine has problems; see the fixes above")
			case host.Warn:
				fmt.Fprintln(a.stdout, "nothing failing; the warnings are worth fixing")
			default:
				fmt.Fprintln(a.stdout, "all good")
			}
			return nil
		},
	}
}

func newHost(a *app) *cobra.Command {
	hostCmd := &cobra.Command{
		Use:   "host",
		Short: "Set up, keep and maintain this machine.",
	}

	var (
		profile  host.Profile
		planOnly bool
	)
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Turn this machine into a bedrock host: Docker, firewall, key-only SSH, updates, swap, registry.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if profile.Hostname == "" {
				current, _ := host.RealEnv().Run(cmd.Context(), "hostname")
				profile.Hostname = strings.TrimSpace(current)
			}
			if err := profile.Validate(); err != nil {
				return err
			}
			return a.operate(cmd.Context(), host.SetupKind, profile, planOnly)
		},
	}
	setup.Flags().StringVar(&profile.Hostname, "hostname", "", "the machine's name (default: keep the current one)")
	setup.Flags().IntVar(&profile.SwapGiB, "swap", 4, "swap file size in GiB; 0 for none")
	setup.Flags().StringVar(&profile.Timezone, "timezone", "", "IANA timezone, such as America/New_York (default: leave it)")
	a.mutatingFlags(setup, &planOnly)

	var reconcilePlan bool
	reconcile := &cobra.Command{
		Use:   "reconcile",
		Short: "Re-apply this machine's profile, repairing whatever drifted.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := host.LoadProfile(host.RealEnv())
			if err != nil {
				return err
			}
			return a.operate(cmd.Context(), host.SetupKind, p, reconcilePlan)
		},
	}
	a.mutatingFlags(reconcile, &reconcilePlan)

	var (
		maintainIn   host.MaintainInput
		maintainPlan bool
	)
	maintain := &cobra.Command{
		Use:   "maintain",
		Short: "Install updates, reboot if allowed and needed, bring everything back and check it.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.operate(cmd.Context(), host.MaintainKind, maintainIn, maintainPlan)
		},
	}
	maintain.Flags().BoolVar(&maintainIn.Reboot, "reboot", false, "reboot when the machine asks for it")
	a.mutatingFlags(maintain, &maintainPlan)

	hostCmd.AddCommand(setup, reconcile, maintain)
	return hostCmd
}

func newUpgrade(a *app) *cobra.Command {
	var planOnly bool
	cmd := &cobra.Command{
		Use:   "upgrade <path-to-new-bedrock>",
		Short: "Replace bedrock with a new build and restart the daemon; systemd rolls back if it can't start.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			return a.operate(cmd.Context(), host.UpgradeKind, host.UpgradeInput{Path: path}, planOnly)
		},
	}
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}
