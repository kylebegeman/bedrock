package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/state"
)

func newDeploy(a *app) *cobra.Command {
	var planOnly bool
	cmd := &cobra.Command{
		Use:   "deploy <source-dir>",
		Short: "Deploy the app in a source directory: build, start, check, switch the edge, retire the old revision.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if _, err := manifest.Load(source); err != nil {
				return err
			}
			revision := time.Now().UTC().Format("20060102-150405")
			return a.operate(cmd.Context(), apps.DeployKind, apps.DeployInput{Source: source, Revision: revision}, planOnly)
		},
	}
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newRollback(a *app) *cobra.Command {
	var (
		planOnly bool
		to       string
	)
	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Make the previous revision active again.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.operate(cmd.Context(), apps.RollbackKind, apps.RollbackInput{App: args[0], Revision: to}, planOnly)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "a specific revision instead of the previous one")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

// openState opens the store read-only for commands that only look.
func (a *app) openState() (*state.Store, error) {
	return state.Open(filepath.Join(a.stateDir, "state.db"))
}

func newPs(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List apps, their workloads and what is running.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			active, err := store.ActiveRevisions(ctx)
			if err != nil {
				return err
			}
			engine, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer engine.Close()
			type row struct {
				App, Workload, Revision, Container, Status, Image string
			}
			var rows []row
			for _, rev := range active {
				var m manifest.Manifest
				if err := json.Unmarshal(rev.Manifest, &m); err != nil {
					return err
				}
				for _, name := range m.WorkloadNames() {
					container := rev.Containers[name]
					status := "missing"
					if info, err := engine.Inspect(ctx, container); err == nil {
						status = info.Status
						if info.Health != "" {
							status += ", " + info.Health
						}
					}
					rows = append(rows, row{rev.App, name, rev.ID, container, status, rev.Images[name]})
				}
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no apps deployed yet")
			} else {
				w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "APP\tWORKLOAD\tREVISION\tSTATUS\tCONTAINER")
				for _, r := range rows {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.App, r.Workload, r.Revision, r.Status, r.Container)
				}
				w.Flush()
			}
			if unowned, err := engine.Unowned(ctx); err == nil && len(unowned) > 0 {
				fmt.Fprintf(a.stdout, "not managed by quark: %d container(s)\n", len(unowned))
			}
			return nil
		},
	}
}

func newLogs(a *app) *cobra.Command {
	var (
		follow bool
		tail   string
	)
	cmd := &cobra.Command{
		Use:   "logs <app> [workload]",
		Short: "Show a workload's output; the app's only workload when there is one.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			container, err := a.resolveContainer(ctx, args[0], optional(args, 1))
			if err != nil {
				return err
			}
			engine, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer engine.Close()
			return engine.Logs(ctx, container, follow, tail, a.stdout)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming")
	cmd.Flags().StringVar(&tail, "tail", "100", "how many lines to start with, or all")
	return cmd
}

func optional(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// resolveContainer finds the active container for an app's workload.
func (a *app) resolveContainer(ctx context.Context, appName, workload string) (string, error) {
	store, err := a.openState()
	if err != nil {
		return "", err
	}
	defer store.Close()
	rev, err := store.RevisionWithStatus(ctx, appName, state.RevisionActive)
	if err != nil {
		return "", err
	}
	if rev == nil {
		return "", fmt.Errorf("%s isn't deployed", appName)
	}
	if workload == "" {
		if len(rev.Containers) != 1 {
			names := make([]string, 0, len(rev.Containers))
			for n := range rev.Containers {
				names = append(names, n)
			}
			return "", fmt.Errorf("%s has several workloads; name one of %v", appName, names)
		}
		for _, c := range rev.Containers {
			return c, nil
		}
	}
	container, ok := rev.Containers[workload]
	if !ok {
		return "", fmt.Errorf("%s has no workload named %s", appName, workload)
	}
	return container, nil
}
