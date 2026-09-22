package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/host"
	"github.com/kylebegeman/bedrock/internal/release"
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
	var (
		planOnly  bool
		toVersion string
		sha256Hex string
	)
	cmd := &cobra.Command{
		Use:   "upgrade [path-to-new-bedrock]",
		Short: "Replace bedrock with a new build and restart the daemon; systemd rolls back if it can't start.",
		Long: "Replace bedrock with a new build and restart the daemon; systemd rolls back if it can't start.\n\n" +
			"With --version the build is downloaded from the published release and checked\n" +
			"against the SHA256SUMS beside it. That proves the two agree and nothing more,\n" +
			"since whoever can replace one can replace both; pass --sha256 with a checksum\n" +
			"from reviewed source to pin it.\n\n" +
			"  bedrock upgrade --version 0.7.4\n" +
			"  bedrock upgrade /tmp/bedrock-new",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0 && toVersion == "":
				return fmt.Errorf("say what to upgrade to: --version 0.7.4, or the path of a build already here")
			case len(args) == 1 && toVersion != "":
				return fmt.Errorf("give either a path or --version, not both")
			}
			path := ""
			if len(args) == 1 {
				if sha256Hex != "" {
					return fmt.Errorf("--sha256 goes with --version; check a local file yourself")
				}
				abs, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				path = abs
			} else {
				if err := release.CheckVersion(toVersion); err != nil {
					return err
				}
				// Into bedrock's own directory, not a temporary one: the
				// daemon restarts mid-upgrade and the plan is rebuilt when
				// the operation resumes, which reads this path again.
				if err := os.MkdirAll(host.LibDir, 0o755); err != nil {
					return err
				}
				fmt.Fprintf(a.stderr, "fetching bedrock %s\n", toVersion)
				got, err := release.Fetch(cmd.Context(), toVersion, host.LibDir, sha256Hex)
				if err != nil {
					return err
				}
				path = got
			}
			return a.operate(cmd.Context(), host.UpgradeKind, host.UpgradeInput{Path: path}, planOnly)
		},
	}
	cmd.Flags().StringVar(&toVersion, "version", "", "a published release to fetch and install, such as 0.7.4")
	cmd.Flags().StringVar(&sha256Hex, "sha256", "", "the expected checksum of that release's build for this machine")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}
