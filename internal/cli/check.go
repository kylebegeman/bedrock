package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

type hostCheckReport struct {
	Valid      bool             `json:"valid"`
	Plan       *kernel.PlanView `json:"plan,omitempty"`
	NotChecked []string         `json:"not_checked"`
}

type sourceCheckReport struct {
	manifest.CheckReport
	HostPreflight *hostCheckReport `json:"host_preflight,omitempty"`
}

func newCheck(a *app) *cobra.Command {
	var name string
	var host bool
	cmd := &cobra.Command{
		Use:   "check [source-dir]",
		Short: "Validate a manifest and its local source inputs without a host.",
		Long: "Validate a manifest and its local source inputs without a host.\n\n" +
			"Checks workload shapes, routes, ports and secret names with the deploy\n" +
			"validator, then checks build contexts, Dockerfiles, static directories\n" +
			"and PostgreSQL init inputs. Nothing is built or changed.\n\n" +
			"This is an offline check. It does not establish DNS, certificates,\n" +
			"image availability, secret values or capacity on a target machine.\n" +
			"Add --host while running on the target to check the same read-only\n" +
			"preconditions as bedrock deploy --plan, including integrations and\n" +
			"route/published-port ownership and availability of named secrets.\n" +
			"It does not execute the plan or prove disk/aggregate resource capacity,\n" +
			"runtime readiness, DNS or certificate issuance.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			report := sourceCheckReport{CheckReport: manifest.CheckSource(dir, name)}
			if report.Valid && host {
				report.HostPreflight = &hostCheckReport{NotChecked: []string{"image builds and pulls", "runtime workload readiness", "DNS and certificate issuance", "disk space and aggregate resource capacity"}}
				plan, err := a.checkHost(cmd.Context(), report.Source, name)
				if err != nil {
					report.Valid = false
					report.Diagnostics = append(report.Diagnostics, manifest.Diagnostic{Code: "host.preflight", Message: err.Error()})
				} else {
					report.HostPreflight.Valid = true
					report.HostPreflight.Plan = plan
				}
			}
			return a.printCheckReport(report, name)
		},
	}
	cmd.Flags().BoolVar(&host, "host", false, "also check deploy-plan preconditions on this target; applies and journals nothing")
	cmd.Flags().StringVar(&name, "manifest", manifest.FileName, "the .yaml or .yml manifest at the source's root")
	return cmd
}

// checkHost uses the same planner as deploy. The fixed revision is only a note
// in its plan: no revision or operation is ever created by Plan.
func (a *app) checkHost(ctx context.Context, source, name string) (*kernel.PlanView, error) {
	runner, err := a.runner(ctx, "check")
	if err != nil {
		return nil, err
	}
	defer runner.Close()
	in := apps.DeployInput{Source: source, Revision: "check", Manifest: name}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	return runner.Plan(ctx, apps.DeployKind, raw)
}

func (a *app) printCheckReport(report sourceCheckReport, name string) error {
	if a.json {
		if err := json.NewEncoder(a.stdout).Encode(report); err != nil {
			return err
		}
	} else if report.Valid {
		fmt.Fprintf(a.stdout, "%s: %s manifest and local source inputs are valid\n", name, report.App)
		if report.HostPreflight != nil {
			fmt.Fprintln(a.stdout, "Host deploy-plan preconditions and secret names passed; runtime readiness remains to be verified during deploy.")
			fmt.Fprintln(a.stdout, "Disk space and aggregate resource capacity are not checked; use bedrock doctor for current host capacity.")
			a.renderer().Plan(report.HostPreflight.Plan)
		} else {
			fmt.Fprintln(a.stdout, "Offline check only; use bedrock check --host on the target before deploying.")
		}
	} else {
		for _, d := range report.Diagnostics {
			location := name
			if d.Line > 0 {
				location += fmt.Sprintf(":%d", d.Line)
				if d.Column > 0 {
					location += fmt.Sprintf(":%d", d.Column)
				}
			}
			field := ""
			if d.Path != "" {
				field = " [" + d.Path + "]"
			}
			fmt.Fprintf(a.stdout, "%s%s: %s\n", location, field, d.Message)
		}
	}
	if !report.Valid {
		return quietError{code: 1}
	}
	return nil
}
