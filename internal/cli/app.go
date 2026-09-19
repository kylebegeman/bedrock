package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apps "github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/state"
)

func newDeploy(a *app) *cobra.Command {
	var (
		planOnly        bool
		restorePostgres string
		restoreVolumes  []string
	)
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
			in := apps.DeployInput{Source: source, Revision: time.Now().UTC().Format("20060102-150405")}
			if restorePostgres != "" {
				if in.Restore.Postgres, err = filepath.Abs(restorePostgres); err != nil {
					return err
				}
			}
			for _, rv := range restoreVolumes {
				name, file, ok := strings.Cut(rv, "=")
				if !ok {
					return fmt.Errorf("--restore-volume wants name=file, not %q", rv)
				}
				if in.Restore.Volumes == nil {
					in.Restore.Volumes = map[string]string{}
				}
				if in.Restore.Volumes[name], err = filepath.Abs(file); err != nil {
					return err
				}
			}
			return a.operate(cmd.Context(), apps.DeployKind, in, planOnly)
		},
	}
	cmd.Flags().StringVar(&restorePostgres, "restore-postgres", "", "a pg_dump file to load into the app's empty database first")
	cmd.Flags().StringArrayVar(&restoreVolumes, "restore-volume", nil, "name=file.tar.gz to unpack into an empty volume first")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newRun(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "run <app> [workload] -- <command...>",
		Short: "Run a one-off command with a workload's image, environment, secrets and volumes.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			if dash < 1 || dash >= len(args) {
				return fmt.Errorf("give the app, optionally the workload, then -- and the command")
			}
			in := apps.RunInput{App: args[0], Workload: optional(args[:dash], 1), Command: args[dash:]}
			a.yes = true
			return a.operate(cmd.Context(), apps.RunKind, in, false)
		},
	}
}

func newJobs(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs <app> [workload]",
		Short: "List recent cron runs and one-off commands.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			runs, err := store.JobRuns(cmd.Context(), args[0], optional(args, 1), limit)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(runs)
			}
			if len(runs) == 0 {
				fmt.Fprintln(a.stdout, "no runs yet")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "WORKLOAD\tKIND\tSTARTED\tTOOK\tRESULT")
			for _, r := range runs {
				result := "running"
				took := ""
				if !r.FinishedAt.IsZero() {
					took = r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond).String()
					result = "ok"
					if r.Error != "" {
						result = r.Error
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Workload, r.Kind, r.StartedAt.Local().Format("2006-01-02 15:04:05"), took, result)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many runs to list")
	return cmd
}

func newPsql(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "psql <app> [-- psql arguments]",
		Short: "Open psql on the app's database.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName := args[0]
			dockerBin, err := exec.LookPath("docker")
			if err != nil {
				return err
			}
			db := strings.ReplaceAll(appName, "-", "_")
			argv := []string{"docker", "exec", "-i"}
			if a.tty {
				argv = append(argv, "-t")
			}
			argv = append(argv, apps.PostgresContainer(appName), "psql", "-U", db, "-d", db)
			argv = append(argv, args[1:]...)
			return syscall.Exec(dockerBin, argv, os.Environ())
		},
	}
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
