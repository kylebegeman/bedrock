package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/restic"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/kylebegeman/bedrock/internal/version"
)

// BackupKind snapshots an app's data into its own bucket: a database
// dump and every volume, deduplicated and encrypted, with retention.
const BackupKind = "app.backup"

// Backup is the Definition for BackupKind.
type Backup struct {
	Store    *state.Store
	Secrets  *secrets.Store
	StateDir string
	// Hostname names the machine, for its own backup.
	Hostname func() string
	// Profile is the machine's profile file, kept in its own backup when
	// there is one.
	Profile string
}

// BackupInput says what to back up: an app, or bedrock's own name for the
// machine's state.
type BackupInput struct {
	App string `json:"app"`
}

// Kind implements kernel.Definition.
func (Backup) Kind() string { return BackupKind }

// MachineTarget is the host name the machine's own snapshots carry.
func MachineTarget(hostname string) string { return "machine-" + hostname }

// openRepo prepares restic for one bucket.
func openRepo(sec *secrets.Store, e *docker.Engine, owner, bucketName string, log func(string)) (restic.Runner, *integration.Storage, error) {
	st, err := integration.LoadStorage(sec)
	if err != nil {
		return restic.Runner{}, nil, fmt.Errorf("backups need the storage integration: %w", err)
	}
	bucket := st.Bucket(bucketName)
	return restic.Runner{Engine: e, Owner: owner, Log: log, Repo: restic.Repo{Repository: st.Repository(bucket), Password: st.Password, Env: st.Env()}}, st, nil
}

// backupRun is the record a backup keeps as its steps go by.
type backupRun struct {
	store *state.Store
	app   string
	kind  string
	id    int64
}

// begin starts a run record, or adopts one a previous attempt left open.
func (r *backupRun) begin(ctx context.Context) error {
	if r.id != 0 {
		return nil
	}
	if last, err := r.store.LastBackupRun(ctx, r.app, r.kind); err == nil && last != nil && !last.Finished() {
		r.id = last.ID
		return nil
	}
	id, err := r.store.StartBackupRun(ctx, r.app, r.kind, time.Now().UTC())
	r.id = id
	return err
}

// fail records a failed run and returns the error.
func (r *backupRun) fail(err error) error {
	if r.id != 0 {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.store.FinishBackupRun(cleanup, r.id, state.BackupRun{FinishedAt: time.Now().UTC(), Error: err.Error()})
	}
	return err
}

// done records a successful run.
func (r *backupRun) done(ctx context.Context, result state.BackupRun) error {
	result.OK = true
	result.FinishedAt = time.Now().UTC()
	return r.store.FinishBackupRun(ctx, r.id, result)
}

// Plan implements kernel.Definition.
func (b Backup) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in BackupInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("backup input: %w", err)
	}
	if in.App == "" {
		return nil, errors.New("which app?")
	}
	st, err := integration.LoadStorage(b.Secrets)
	if err != nil {
		return nil, fmt.Errorf("backups need the storage integration: %w", err)
	}
	if in.App == manifest.ReservedApp {
		return b.machinePlan(st)
	}
	rev, err := b.Store.RevisionWithStatus(ctx, in.App, state.RevisionActive)
	if err != nil {
		return nil, err
	}
	if rev == nil {
		return nil, fmt.Errorf("%s isn't deployed", in.App)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return nil, err
	}
	if !m.HasData() {
		return nil, fmt.Errorf("%s keeps no data; there is nothing to back up", in.App)
	}
	if m.Backup != nil && m.Backup.Off {
		return nil, fmt.Errorf("backups are off for %s in its bedrock.yaml", in.App)
	}
	return b.appPlan(st, rev, &m)
}

func (b Backup) appPlan(st *integration.Storage, rev *state.Revision, m *manifest.Manifest) (*kernel.Plan, error) {
	app := rev.App
	staging := filepath.Join(b.StateDir, "backups", app)
	bucket := st.Bucket(app)
	keep := m.KeepPolicy()
	run := &backupRun{store: b.Store, app: app, kind: state.BackupRunBackup}
	var summary *restic.Summary
	plan := &kernel.Plan{Target: app, Recovery: kernel.Resume}

	prepare := "write the manifest"
	if m.PostgresVersion() != "" {
		prepare += " and preserve its initialization scripts"
	}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "prepare", Change: prepare,
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			if err := resetDir(staging); err != nil {
				return run.fail(err)
			}
			if err := os.WriteFile(filepath.Join(staging, "manifest.json"), append(rev.Manifest, '\n'), 0o600); err != nil {
				return run.fail(err)
			}
			// The volumes mount over these inside the read-only tree, so
			// the mountpoints have to be there already.
			for _, v := range m.DataVolumes() {
				if err := os.MkdirAll(filepath.Join(staging, "volumes", v), 0o700); err != nil {
					return run.fail(err)
				}
			}
			if m.PostgresVersion() == "" {
				return nil
			}
			// The database's first-run scripts go with it: a restore on a
			// new machine runs them again before it loads the dump.
			if init := InitDir(b.StateDir, app); keepsOwners(m) && dirExists(init) {
				if out, err := exec.CommandContext(ctx, "cp", "-a", init, filepath.Join(staging, initSnapshotDir)).CombinedOutput(); err != nil {
					return run.fail(fmt.Errorf("copy the database's first-run scripts: %s", strings.TrimSpace(string(out))))
				}
			}
			return nil
		},
	})
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "snapshot", Change: fmt.Sprintf("pause writers, dump and snapshot consistent data into bucket %s, then resume (30m limit)", bucket),
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			r, _, err := openRepo(b.Secrets, e, app, app, nil)
			if err != nil {
				return run.fail(err)
			}
			created, err := r.Ensure(ctx)
			if err != nil {
				return run.fail(err)
			}
			if created {
				fmt.Fprintf(out, "made bucket %s and its repository\n", bucket)
			}
			mounts := []string{restic.DirMount(staging, true)}
			for _, v := range m.DataVolumes() {
				mounts = append(mounts, restic.VolumeMount(docker.VolumeName(app, v), v, true))
			}
			err = withQuiescedWriters(ctx, e, b.Store.Path(), m, out, func(ctx context.Context) error {
				if m.PostgresVersion() != "" {
					values, _, err := b.Secrets.LoadCurrent(app)
					if err != nil {
						return err
					}
					svc, err := postgresService(m, values, "")
					if err != nil {
						return err
					}
					size, err := dumpDatabase(ctx, e, svc, filepath.Join(staging, "postgres.dump"))
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "database dumped: %s\n", HumanBytes(size))
				}
				var err error
				summary, err = r.Backup(ctx, app, mounts, restic.DataRoot)
				return err
			})
			if err != nil {
				return run.fail(err)
			}
			_ = b.Store.RecordIntegrationUse(ctx, integration.StorageName, "backup "+app, time.Now().UTC())
			fmt.Fprintf(out, "snapshot %s: %d files, %s, %s new\n", short(summary.SnapshotID), summary.TotalFiles, HumanBytes(summary.TotalBytes), HumanBytes(summary.DataAdded))
			return nil
		},
	})
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "retention", Change: fmt.Sprintf("keep %d daily, %d weekly and %d monthly snapshots", keep.Daily, keep.Weekly, keep.Monthly),
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			r, _, err := openRepo(b.Secrets, e, app, app, nil)
			if err != nil {
				return run.fail(err)
			}
			removed, err := r.Forget(ctx, app, restic.Keep{Daily: keep.Daily, Weekly: keep.Weekly, Monthly: keep.Monthly})
			if err != nil {
				return run.fail(err)
			}
			if summary == nil {
				// Resumed after the snapshot step: take the newest.
				latest, err := r.Latest(ctx, app)
				if err != nil {
					return run.fail(err)
				}
				summary = &restic.Summary{SnapshotID: latest.ID, BackupStart: latest.Time}
				if latest.Summary != nil {
					summary.TotalFiles, summary.TotalBytes = latest.Summary.TotalFiles, latest.Summary.TotalBytes
				}
			}
			detail := fmt.Sprintf("%d files, %s", summary.TotalFiles, HumanBytes(summary.TotalBytes))
			if m.PostgresVersion() != "" {
				detail = "database and " + detail
			}
			if err := run.done(ctx, state.BackupRun{Snapshot: short(summary.SnapshotID), SnapshotAt: summary.BackupStart, Files: summary.TotalFiles, Bytes: summary.TotalBytes, Detail: detail}); err != nil {
				return err
			}
			_ = os.RemoveAll(staging)
			if removed > 0 {
				fmt.Fprintf(out, "removed %d old snapshot(s)\n", removed)
			} else {
				fmt.Fprintln(out, "nothing old enough to remove")
			}
			return nil
		},
	})
	return plan, nil
}

// machinePlan backs up what makes this machine itself: the state store,
// the sealed secrets and the host profile, into a bucket of its own. The
// secrets key stays on the machine; the recovery identity opens them.
func (b Backup) machinePlan(st *integration.Storage) (*kernel.Plan, error) {
	hostname := b.Hostname()
	target := MachineTarget(hostname)
	staging := filepath.Join(b.StateDir, "backups", "machine")
	bucket := st.Bucket(target)
	run := &backupRun{store: b.Store, app: manifest.ReservedApp, kind: state.BackupRunBackup}
	var summary *restic.Summary
	plan := &kernel.Plan{Target: "this machine", Recovery: kernel.Resume}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "prepare", Change: "copy the state store, the sealed secrets and the host profile",
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			if err := resetDir(staging); err != nil {
				return run.fail(err)
			}
			if err := b.Store.BackupTo(ctx, filepath.Join(staging, "state.db")); err != nil {
				return run.fail(fmt.Errorf("copy the state store: %w", err))
			}
			if secretsDir := filepath.Join(b.StateDir, "secrets"); dirExists(secretsDir) {
				if out, err := exec.CommandContext(ctx, "cp", "-a", secretsDir, filepath.Join(staging, "secrets")).CombinedOutput(); err != nil {
					return run.fail(fmt.Errorf("copy the secrets: %s", strings.TrimSpace(string(out))))
				}
			}
			for _, f := range []string{b.Profile, b.Secrets.KeyPath + ".pub"} {
				if f == "" {
					continue
				}
				if data, err := os.ReadFile(f); err == nil {
					if err := os.WriteFile(filepath.Join(staging, filepath.Base(f)), data, 0o600); err != nil {
						return run.fail(err)
					}
				}
			}
			info, _ := json.MarshalIndent(map[string]any{"hostname": hostname, "bedrock": version.Current().Version, "taken_at": time.Now().UTC()}, "", "  ")
			if err := os.WriteFile(filepath.Join(staging, "machine.json"), append(info, '\n'), 0o600); err != nil {
				return run.fail(err)
			}
			fmt.Fprintln(out, "state, secrets and profile copied")
			return nil
		},
	})
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "snapshot", Change: fmt.Sprintf("snapshot them into bucket %s", bucket),
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			r, _, err := openRepo(b.Secrets, e, manifest.ReservedApp, target, nil)
			if err != nil {
				return run.fail(err)
			}
			if created, err := r.Ensure(ctx); err != nil {
				return run.fail(err)
			} else if created {
				fmt.Fprintf(out, "made bucket %s and its repository\n", bucket)
			}
			summary, err = r.Backup(ctx, target, []string{restic.DirMount(staging, true)}, restic.DataRoot)
			if err != nil {
				return run.fail(err)
			}
			_ = b.Store.RecordIntegrationUse(ctx, integration.StorageName, "backup of the machine", time.Now().UTC())
			fmt.Fprintf(out, "snapshot %s: %d files, %s\n", short(summary.SnapshotID), summary.TotalFiles, HumanBytes(summary.TotalBytes))
			return nil
		},
	})
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "retention", Change: "keep 7 daily, 4 weekly and 6 monthly snapshots",
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			r, _, err := openRepo(b.Secrets, e, manifest.ReservedApp, target, nil)
			if err != nil {
				return run.fail(err)
			}
			removed, err := r.Forget(ctx, target, restic.Keep{Daily: 7, Weekly: 4, Monthly: 6})
			if err != nil {
				return run.fail(err)
			}
			if summary == nil {
				latest, err := r.Latest(ctx, target)
				if err != nil {
					return run.fail(err)
				}
				summary = &restic.Summary{SnapshotID: latest.ID, BackupStart: latest.Time}
			}
			if err := run.done(ctx, state.BackupRun{Snapshot: short(summary.SnapshotID), SnapshotAt: summary.BackupStart, Files: summary.TotalFiles, Bytes: summary.TotalBytes, Detail: "state, secrets and profile"}); err != nil {
				return err
			}
			_ = os.RemoveAll(staging)
			if removed > 0 {
				fmt.Fprintf(out, "removed %d old snapshot(s)\n", removed)
			}
			return nil
		},
	})
	return plan, nil
}

// dumpDatabase writes an app's database as a pg_dump custom-format file.
func dumpDatabase(ctx context.Context, e *docker.Engine, p pgService, path string) (int64, error) {
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	stderr, err := e.ExecToEnv(ctx, p.Container, map[string]string{"PGPASSWORD": p.Password}, f, "pg_dump", "-Fc", "-h", "127.0.0.1", "-U", p.User, "-d", p.Database)
	f.Close()
	if err != nil {
		_ = os.Remove(path + ".tmp")
		return 0, fmt.Errorf("pg_dump: %w\n%s", err, tail(stderr, 3))
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func resetDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

func dirExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// short is a snapshot's id as restic shows it: its first eight characters.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// HumanBytes says a size the way people do.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
