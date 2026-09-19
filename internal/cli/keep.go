package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apps "github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/integration"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/signals"
	"github.com/kylebegeman/quark/internal/state"
	"github.com/kylebegeman/quark/internal/version"
	"github.com/kylebegeman/quark/internal/watch"
)

// appRow is one app as ls and status see it.
type appRow struct {
	Name        string    `json:"name"`
	Owner       string    `json:"owner,omitempty"`
	Description string    `json:"description,omitempty"`
	Repo        string    `json:"repo,omitempty"`
	Hosts       []string  `json:"hosts,omitempty"`
	Revision    string    `json:"revision"`
	Commit      string    `json:"commit,omitempty"`
	DeployedAt  time.Time `json:"deployed_at"`
	// BackedUp says the app has data and backups aren't off.
	BackedUp   bool             `json:"backed_up"`
	LastBackup *state.BackupRun `json:"last_backup,omitempty"`
	GoodBackup *state.BackupRun `json:"good_backup,omitempty"`
	LastDrill  *state.BackupRun `json:"last_drill,omitempty"`
	manifest   manifest.Manifest
	containers map[string]string
}

// appRows reads every deployed app.
func appRows(ctx context.Context, store *state.Store) ([]appRow, error) {
	active, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	var rows []appRow
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			return nil, err
		}
		row := appRow{Name: rev.App, Owner: m.Owner, Description: m.Description, Repo: m.Repo, Hosts: m.Hosts(), Revision: rev.ID, Commit: rev.Source, DeployedAt: rev.CreatedAt, BackedUp: m.BackedUp(), manifest: m, containers: rev.Containers}
		row.LastBackup, _ = store.LastBackupRun(ctx, rev.App, state.BackupRunBackup)
		row.GoodBackup, _ = store.LastGoodBackupRun(ctx, rev.App, state.BackupRunBackup)
		row.LastDrill, _ = store.LastBackupRun(ctx, rev.App, state.BackupRunDrill)
		rows = append(rows, row)
	}
	return rows, nil
}

// backupWord sums up an app's backups in a few words.
func backupWord(r appRow, now time.Time) string {
	if !r.BackedUp {
		if r.manifest.HasData() {
			return "off"
		}
		return "no data"
	}
	if r.GoodBackup == nil {
		if r.LastBackup != nil && r.LastBackup.Finished() {
			return "FAILED: " + r.LastBackup.Error
		}
		return "never"
	}
	word := agoWord(r.GoodBackup.StartedAt, now)
	if r.LastBackup != nil && r.LastBackup.ID != r.GoodBackup.ID && r.LastBackup.Finished() && !r.LastBackup.OK {
		word += ", then FAILED"
	}
	return word
}

// drillWord sums up an app's last drill.
func drillWord(r appRow, now time.Time) string {
	switch {
	case !r.BackedUp:
		return ""
	case r.LastDrill == nil:
		return "none yet"
	case !r.LastDrill.Finished():
		return "running"
	case r.LastDrill.OK:
		return "ok " + agoWord(r.LastDrill.StartedAt, now)
	default:
		return "FAILED " + agoWord(r.LastDrill.StartedAt, now)
	}
}

func agoWord(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.0f h ago", d.Hours())
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

func newLs(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the apps on this machine: owner, hosts, repository, last deploy, last backup.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			rows, err := appRows(ctx, store)
			if err != nil {
				return err
			}
			var unmanaged []string
			if engine, err := docker.Connect(ctx); err == nil {
				unmanaged, _ = engine.Unowned(ctx)
				engine.Close()
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Apps      []appRow `json:"apps"`
					Unmanaged []string `json:"unmanaged,omitempty"`
				}{rows, unmanaged})
			}
			now := time.Now().UTC()
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no apps deployed yet")
			} else {
				w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "APP\tOWNER\tHOSTS\tREPO\tDEPLOYED\tBACKUP")
				for _, r := range rows {
					deployed := agoWord(r.DeployedAt, now)
					if r.Commit != "" {
						deployed += " (" + short7(r.Commit) + ")"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, orDash(r.Owner), orDash(strings.Join(r.Hosts, ", ")), orDash(r.Repo), deployed, backupWord(r, now))
				}
				w.Flush()
				for _, r := range rows {
					if r.Description != "" {
						fmt.Fprintf(a.stdout, "  %s: %s\n", r.Name, r.Description)
					}
				}
			}
			if len(unmanaged) > 0 {
				sort.Strings(unmanaged)
				fmt.Fprintf(a.stdout, "not managed by quark: %s\n", strings.Join(unmanaged, ", "))
			}
			return nil
		},
	}
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// machineLine says how the machine is doing, in one line.
func machineLine(ctx context.Context, hostname string, open []state.Incident) string {
	parts := []string{"quark " + version.Current().Version}
	if engine, err := docker.Connect(ctx); err != nil {
		parts = append(parts, "DOCKER NOT ANSWERING")
	} else {
		if info, err := engine.Inspect(ctx, edge.Container); err == nil && info.Running {
			parts = append(parts, "edge ok")
		} else {
			parts = append(parts, "EDGE NOT RUNNING")
		}
		engine.Close()
	}
	if free, total, ok := watch.DiskFree("/"); ok {
		parts = append(parts, fmt.Sprintf("%s free of %s", watch.HumanBytes(free), watch.HumanBytes(total)))
	}
	if avail, total, ok := watch.Memory(); ok {
		parts = append(parts, fmt.Sprintf("%s of %s memory available", watch.HumanBytes(avail), watch.HumanBytes(total)))
	}
	if load, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(load)); len(f) >= 3 {
			parts = append(parts, "load "+f[0]+" "+f[1]+" "+f[2])
		}
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		parts = append(parts, "REBOOT REQUIRED")
	}
	alerts := "no open alerts"
	if len(open) == 1 {
		alerts = "1 OPEN ALERT: " + open[0].Subject
	} else if len(open) > 1 {
		alerts = fmt.Sprintf("%d OPEN ALERTS", len(open))
	}
	parts = append(parts, alerts)
	return hostname + ": " + strings.Join(parts, ", ")
}

func newStatus(a *app) *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "status [app]",
		Short: "How the machine and its apps are doing: health, traffic, resources, backups, alerts.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			now := time.Now().UTC()
			rows, err := appRows(ctx, store)
			if err != nil {
				return err
			}
			open, err := store.OpenIncidents(ctx)
			if err != nil {
				return err
			}
			openBy := map[string]state.Incident{}
			for _, inc := range open {
				openBy[inc.Key] = inc
			}
			sums, err := signals.AppSummaries(ctx, store, now.Add(-since))
			if err != nil {
				return err
			}
			hostname, _ := os.Hostname()
			if len(args) == 1 {
				for _, r := range rows {
					if r.Name == args[0] {
						return a.appStatus(ctx, store, r, sums, openBy, now, since)
					}
				}
				return fmt.Errorf("%s isn't deployed", args[0])
			}
			if a.json {
				type row struct {
					appRow
					Health   string          `json:"health"`
					Signals  signals.Summary `json:"signals"`
					Incident *state.Incident `json:"incident,omitempty"`
				}
				out := make([]row, 0, len(rows))
				for _, r := range rows {
					health, inc := healthWord(r, openBy)
					out = append(out, row{r, health, sums[r.Name], inc})
				}
				return json.NewEncoder(a.stdout).Encode(struct {
					Machine string           `json:"machine"`
					Apps    []row            `json:"apps"`
					Open    []state.Incident `json:"open_alerts"`
				}{machineLine(ctx, hostname, open), out, open})
			}
			fmt.Fprintln(a.stdout, machineLine(ctx, hostname, open))
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no apps deployed yet")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(w, "APP\tHEALTH\tREQ/%s\tERRORS\tP95\tCPU\tMEMORY\tDISK\tBACKUP\tDRILL\n", strings.ToUpper(windowWord(since)))
			for _, r := range rows {
				health, _ := healthWord(r, openBy)
				s := sums[r.Name]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, health, count(s.Requests), count(s.Errors), latency(s.P95MS, s.Requests), percent(s.CPUPercent, s.Hours), bytesWord(s.MemoryBytes), bytesWord(s.DiskBytes), backupWord(r, now), orDash(drillWord(r, now)))
			}
			w.Flush()
			for _, inc := range open {
				fmt.Fprintf(a.stdout, "ALERT %s since %s: %s\n", inc.Subject, inc.OpenedAt.Local().Format("15:04"), inc.Message)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "the window for traffic and resources")
	return cmd
}

// appStatus shows one app in detail.
func (a *app) appStatus(ctx context.Context, store *state.Store, r appRow, sums map[string]signals.Summary, openBy map[string]state.Incident, now time.Time, since time.Duration) error {
	health, inc := healthWord(r, openBy)
	rows, _ := store.Signals(ctx, r.Name, now.Add(-since))
	if a.json {
		return json.NewEncoder(a.stdout).Encode(struct {
			appRow
			Health   string          `json:"health"`
			Signals  signals.Summary `json:"signals"`
			Incident *state.Incident `json:"incident,omitempty"`
			Rollups  []state.Signal  `json:"rollups"`
		}{r, health, sums[r.Name], inc, rows})
	}
	fmt.Fprintf(a.stdout, "%s: %s\n", r.Name, health)
	if inc != nil {
		fmt.Fprintf(a.stdout, "  since %s: %s\n", inc.OpenedAt.Local().Format("2006-01-02 15:04"), inc.Message)
	}
	if r.Description != "" {
		fmt.Fprintf(a.stdout, "  %s\n", r.Description)
	}
	deployed := agoWord(r.DeployedAt, now)
	if r.Commit != "" {
		deployed += ", commit " + short7(r.Commit)
	}
	fmt.Fprintf(a.stdout, "  revision %s, deployed %s\n", r.Revision, deployed)
	if len(r.Hosts) > 0 {
		fmt.Fprintf(a.stdout, "  hosts: %s\n", strings.Join(r.Hosts, ", "))
	}
	s := sums[r.Name]
	fmt.Fprintf(a.stdout, "  last %s: %s requests, %s errors, p95 %s, %s sent; cpu %s, memory %s (peak %s), disk %s\n", windowWord(since), count(s.Requests), count(s.Errors), latency(s.P95MS, s.Requests), bytesWord(s.Bytes), percent(s.CPUPercent, s.Hours), bytesWord(s.MemoryBytes), bytesWord(s.MemoryMax), bytesWord(s.DiskBytes))
	byHost := map[string][]state.Signal{}
	byWorkload := map[string][]state.Signal{}
	for _, sig := range rows {
		switch sig.Metric {
		case state.SignalCPUPercent, state.SignalMemoryBytes, state.SignalMemoryMax:
			byWorkload[sig.Key] = append(byWorkload[sig.Key], sig)
		case state.SignalDiskBytes:
		default:
			byHost[sig.Key] = append(byHost[sig.Key], sig)
		}
	}
	for _, h := range sortedKeys(byHost) {
		hs := signals.Summarize(byHost[h])
		fmt.Fprintf(a.stdout, "    %s: %s requests, %s errors, p95 %s\n", h, count(hs.Requests), count(hs.Errors), latency(hs.P95MS, hs.Requests))
	}
	for _, wl := range sortedKeys(byWorkload) {
		ws := signals.Summarize(byWorkload[wl])
		fmt.Fprintf(a.stdout, "    %s: cpu %s, memory %s (peak %s)\n", wl, percent(ws.CPUPercent, ws.Hours), bytesWord(ws.MemoryBytes), bytesWord(ws.MemoryMax))
	}
	if r.BackedUp {
		fmt.Fprintf(a.stdout, "  backups: %s; drill %s\n", backupWord(r, now), orDash(drillWord(r, now)))
		if r.GoodBackup != nil {
			fmt.Fprintf(a.stdout, "    last good snapshot %s from %s: %s\n", r.GoodBackup.Snapshot, r.GoodBackup.SnapshotAt.Local().Format("2006-01-02 15:04"), r.GoodBackup.Detail)
		}
		if r.LastDrill != nil && r.LastDrill.Finished() {
			fmt.Fprintf(a.stdout, "    last drill: %s\n", orDash(firstNonEmpty(r.LastDrill.Detail, r.LastDrill.Error)))
		}
	} else if r.manifest.HasData() {
		fmt.Fprintln(a.stdout, "  backups: off in quark.yaml")
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// healthWord is an app's health as the watcher last saw it.
func healthWord(r appRow, openBy map[string]state.Incident) (string, *state.Incident) {
	if inc, ok := openBy["app:"+r.Name]; ok {
		word := "PROBLEM"
		if inc.Severity == state.SeverityCritical {
			word = "DOWN"
		}
		return word, &inc
	}
	n := len(r.containers)
	if n == 1 {
		return "ok, 1 container", nil
	}
	return fmt.Sprintf("ok, %d containers", n), nil
}

func windowWord(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		days := int(d.Hours() / 24)
		if days == 1 {
			return "24h"
		}
		return fmt.Sprintf("%dd", days)
	}
	return d.String()
}

func count(n int64) string {
	s := fmt.Sprint(n)
	if n < 1000 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func latency(ms float64, requests int64) string {
	if requests == 0 {
		return "-"
	}
	if ms >= 1000 {
		return fmt.Sprintf("%.1f s", ms/1000)
	}
	return fmt.Sprintf("%.0f ms", ms)
}

func percent(p float64, hours int) string {
	if hours == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", p)
}

func bytesWord(n int64) string {
	if n <= 0 {
		return "-"
	}
	return watch.HumanBytes(uint64(n))
}

func newBackup(a *app) *cobra.Command {
	var planOnly bool
	cmd := &cobra.Command{
		Use:   "backup <app>",
		Short: "Back the app's data up to its bucket now. \"quark\" backs up the machine's own state.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.operate(cmd.Context(), apps.BackupKind, apps.BackupInput{App: args[0]}, planOnly)
		},
	}
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newDrill(a *app) *cobra.Command {
	var (
		planOnly bool
		snapshot string
	)
	cmd := &cobra.Command{
		Use:   "drill <app>",
		Short: "Prove a backup: restore it beside the app, start the app on it, check, clean up.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.operate(cmd.Context(), apps.DrillKind, apps.DrillInput{App: args[0], Snapshot: snapshot}, planOnly)
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "a snapshot id instead of the latest")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newRestore(a *app) *cobra.Command {
	var (
		planOnly bool
		snapshot string
	)
	cmd := &cobra.Command{
		Use:   "restore <app>",
		Short: "Bring an app's data back from its bucket onto this machine, before deploying it here.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.operate(cmd.Context(), apps.RestoreKind, apps.RestoreInput{App: args[0], Snapshot: snapshot}, planOnly)
		},
	}
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "a snapshot id instead of the latest")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newBackups(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "backups [app]",
		Short: "List backups, drills and restores, newest first.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			runs, err := store.BackupRuns(cmd.Context(), optional(args, 0), "", limit)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(runs)
			}
			if len(runs) == 0 {
				fmt.Fprintln(a.stdout, "no backups yet")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tKIND\tSTARTED\tTOOK\tRESULT\tSNAPSHOT\tDETAIL")
			for _, r := range runs {
				result, took, detail := "running", "", r.Detail
				if r.Finished() {
					took = r.FinishedAt.Sub(r.StartedAt).Round(time.Second).String()
					if r.OK {
						result = "ok"
					} else {
						result, detail = "FAILED", r.Error
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.App, r.Kind, r.StartedAt.Local().Format("2006-01-02 15:04:05"), took, result, orDash(r.Snapshot), detail)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many to list")
	return cmd
}

func newAlerts(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "What is wrong right now, and what was recently.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			all, err := store.Incidents(ctx, limit)
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(all)
			}
			var open, past []state.Incident
			for _, inc := range all {
				if inc.Open() {
					open = append(open, inc)
				} else {
					past = append(past, inc)
				}
			}
			if len(open) == 0 {
				fmt.Fprintln(a.stdout, "nothing wrong right now")
			}
			for _, inc := range open {
				fmt.Fprintf(a.stdout, "OPEN  %s (%s) since %s: %s\n", inc.Subject, inc.Severity, inc.OpenedAt.Local().Format("2006-01-02 15:04"), inc.Message)
				switch {
				case inc.NotifyError != "":
					fmt.Fprintf(a.stdout, "      NOT DELIVERED: %s\n", inc.NotifyError)
				case !inc.NotifiedAt.IsZero():
					fmt.Fprintf(a.stdout, "      told you at %s\n", inc.NotifiedAt.Local().Format("15:04"))
				}
			}
			for _, inc := range past {
				fmt.Fprintf(a.stdout, "past  %s: %s, from %s for %s\n", inc.Subject, inc.Message, inc.OpenedAt.Local().Format("2006-01-02 15:04"), inc.ResolvedAt.Sub(inc.OpenedAt).Round(time.Minute))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many to list, open ones included")
	test := &cobra.Command{
		Use:   "test",
		Short: "Send a test alert through the email integration.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			hostname, _ := os.Hostname()
			n := watch.EmailNotifier{Secrets: a.secretsStore(), Store: store}
			if err := n.Test(cmd.Context(), hostname); err != nil {
				return err
			}
			cfg, _ := integration.LoadEmail(a.secretsStore())
			fmt.Fprintf(a.stdout, "test alert sent to %s\n", strings.Join(cfg.To, ", "))
			return nil
		},
	}
	cmd.AddCommand(test)
	return cmd
}

func newWatch(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Public URLs this machine checks from the outside, such as the other machine's sites.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			ws, err := store.Watches(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(ws)
			}
			if len(ws) == 0 {
				fmt.Fprintln(a.stdout, "watching nothing; add a URL with quark watch add https://...")
			}
			for _, w := range ws {
				fmt.Fprintf(a.stdout, "%s (since %s)\n", w.URL, w.AddedAt.Local().Format("2006-01-02"))
			}
			return nil
		},
	}
	add := &cobra.Command{
		Use:   "add <url>",
		Short: "Check a URL every minute and alert when it stops answering 200.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
				return errors.New("the URL must start with https:// or http://")
			}
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.AddWatch(cmd.Context(), url, time.Now().UTC()); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "watching %s\n", url)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove <url>",
		Short: "Stop checking a URL.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.RemoveWatch(cmd.Context(), args[0]); errors.Is(err, state.ErrNotFound) {
				return fmt.Errorf("%s wasn't being watched", args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "no longer watching %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, remove)
	return cmd
}

func newIntegration(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "integration",
		Short: "The credentials quark itself uses: storage for backups, email for alerts, Cloudflare for DNS.",
	}
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "Type an integration's values in, or pipe field=value lines. Values are sealed and never shown.",
		Long:  "Integrations: " + strings.Join(integration.Names(), ", ") + ". In a terminal each field is asked for; otherwise stdin holds one field=value per line.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			def, ok := integration.Lookup(args[0])
			if !ok {
				return fmt.Errorf("no integration named %q; there are %s", args[0], strings.Join(integration.Names(), ", "))
			}
			store := a.secretsStore()
			if err := a.ensureSecretsKey(store); err != nil {
				return err
			}
			values, err := readIntegrationValues(a, def)
			if err != nil {
				return err
			}
			version, generated, err := integration.Set(store, def.Name, values)
			if err != nil {
				return err
			}
			for field, value := range generated {
				fmt.Fprintf(a.stderr, "\nquark made a %s for %s. It is shown once, here, and nowhere else. Keep it with the recovery identity in your password manager; a new machine needs it to read these backups:\n\n  %s\n\n", field, def.Name, value)
			}
			fmt.Fprintf(a.stdout, "%s set (quark's secrets version %d)\n", def.Name, version)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the integrations, which are set, and when they were last used.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			statuses, err := integration.List(a.secretsStore())
			if err != nil {
				return err
			}
			uses := map[string]state.IntegrationUse{}
			if store, err := a.openState(); err == nil {
				uses, _ = store.IntegrationUses(cmd.Context())
				store.Close()
			}
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Integrations []integration.Status            `json:"integrations"`
					Uses         map[string]state.IntegrationUse `json:"uses"`
				}{statuses, uses})
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tFOR\tSET\tFIELDS\tLAST USED")
			for _, st := range statuses {
				set, fields, used := "no", "-", "-"
				if st.Set {
					set = st.At.Local().Format("2006-01-02 15:04")
					var fs []string
					for _, k := range sortedKeys(st.Fields) {
						fs = append(fs, k+"="+st.Fields[k])
					}
					fields = strings.Join(fs, " ")
				}
				if u, ok := uses[st.Name]; ok {
					used = fmt.Sprintf("%s (%s)", agoWord(u.UsedAt, time.Now().UTC()), u.Purpose)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", st.Name, st.Purpose, set, fields, used)
			}
			return w.Flush()
		},
	}
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Drop an integration's values.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			version, err := integration.Remove(a.secretsStore(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s removed (quark's secrets version %d)\n", args[0], version)
			return nil
		},
	}
	cmd.AddCommand(set, list, remove)
	return cmd
}

// readIntegrationValues asks for each field in a terminal, hiding
// secrets, or reads field=value lines from stdin otherwise.
func readIntegrationValues(a *app, def integration.Definition) (map[string]string, error) {
	values := map[string]string{}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, fmt.Errorf("expected field=value, got %q", line)
			}
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		return values, sc.Err()
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(a.stderr, "%s: %s\n", def.Name, def.Purpose)
	for _, f := range def.Fields {
		prompt := f.Prompt
		if f.Default != "" {
			prompt += " [" + f.Default + "]"
		} else if f.Optional {
			prompt += " (optional)"
		}
		fmt.Fprintf(a.stderr, "  %s: ", prompt)
		if f.Secret {
			raw, err := term.ReadPassword(fd)
			fmt.Fprintln(a.stderr)
			if err != nil {
				return nil, err
			}
			values[f.Name] = string(raw)
			continue
		}
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return nil, err
		}
		values[f.Name] = strings.TrimSpace(line)
	}
	return values, nil
}
