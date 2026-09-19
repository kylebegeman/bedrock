package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/restic"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/state"
)

// RestoreKind brings an app's data back from its bucket onto this
// machine, before the app is deployed here: the real thing, for a new
// machine or after a loss. The app's volumes and database must be empty.
const RestoreKind = "app.restore"

// RestoreDef is the Definition for RestoreKind.
type RestoreDef struct {
	Store    *state.Store
	Secrets  *secrets.Store
	StateDir string
}

// RestoreInput says which app and which snapshot; empty means the latest.
type RestoreInput struct {
	App      string `json:"app"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Kind implements kernel.Definition.
func (RestoreDef) Kind() string { return RestoreKind }

// Plan implements kernel.Definition.
func (r RestoreDef) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in RestoreInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("restore input: %w", err)
	}
	if in.App == "" {
		return nil, errors.New("which app?")
	}
	if in.App == manifest.ReservedApp {
		return nil, errors.New("the machine's own backup is restored by hand: fetch the snapshot with restic and open the secrets with the recovery identity")
	}
	if in.Snapshot == "" {
		in.Snapshot = "latest"
	}
	if rev, err := r.Store.RevisionWithStatus(ctx, in.App, state.RevisionActive); err != nil {
		return nil, err
	} else if rev != nil {
		return nil, fmt.Errorf("%s is deployed on this machine; a restore is for a machine that doesn't run it yet (quark remove %s --data first), or run quark drill %s to test the backup", in.App, in.App, in.App)
	}
	app := in.App
	staging := filepath.Join(r.StateDir, "restore", app)
	run := &backupRun{store: r.Store, app: app, kind: state.BackupRunRestore}
	var (
		m        *manifest.Manifest
		snapshot *restic.Snapshot
	)
	loadStaged := func() (*manifest.Manifest, error) {
		if m != nil {
			return m, nil
		}
		data, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("the fetched manifest is missing: %w", err)
		}
		var parsed manifest.Manifest
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, err
		}
		m = &parsed
		return m, nil
	}
	plan := &kernel.Plan{Target: app, Recovery: kernel.Resume}
	add := func(st kernel.Step) { plan.Steps = append(plan.Steps, st) }

	add(kernel.Step{
		Name: "fetch", Change: fmt.Sprintf("fetch snapshot %s's manifest and database dump from the app's bucket", in.Snapshot),
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			rs, _, err := openRepo(r.Secrets, e, app, app, nil)
			if err != nil {
				return run.fail(err)
			}
			if in.Snapshot == "latest" {
				snapshot, err = rs.Latest(ctx, app)
				if errors.Is(err, restic.ErrNoRepository) {
					return run.fail(fmt.Errorf("no backups of %s in the storage this machine is set up with", app))
				}
				if err != nil {
					return run.fail(err)
				}
			}
			if err := resetDir(staging); err != nil {
				return run.fail(err)
			}
			id := in.Snapshot
			if snapshot != nil {
				id = snapshot.ID
			}
			if _, err := rs.Restore(ctx, id, app, []string{restic.DirMount(staging, false)}, restic.DataRoot+"/manifest.json", restic.DataRoot+"/postgres.dump", restic.DataRoot+"/"+initSnapshotDir); err != nil {
				return run.fail(err)
			}
			mf, err := loadStaged()
			if err != nil {
				return run.fail(err)
			}
			when := ""
			if snapshot != nil {
				when = " from " + snapshot.Time.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(out, "snapshot %s%s: %s\n", short(id), when, describeData(mf))
			return nil
		},
	})
	add(kernel.Step{
		Name: "volumes", Change: "restore the volumes the snapshot holds into empty volumes",
		Apply: func(ctx context.Context, out io.Writer) error {
			mf, err := loadStaged()
			if err != nil {
				return run.fail(err)
			}
			if len(mf.DataVolumes()) == 0 {
				fmt.Fprintln(out, "no volumes")
				return nil
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			var mounts, includes []string
			for _, v := range mf.DataVolumes() {
				vol := docker.VolumeName(app, v)
				if err := e.EnsureVolume(ctx, vol); err != nil {
					return run.fail(err)
				}
				empty, err := e.VolumeEmpty(ctx, vol)
				if err != nil {
					return run.fail(err)
				}
				if !empty {
					return run.fail(fmt.Errorf("volume %s already has files; a restore is for an empty volume", v))
				}
				mounts = append(mounts, restic.VolumeMount(vol, v, false))
				includes = append(includes, restic.VolumePath(v))
			}
			rs, _, err := openRepo(r.Secrets, e, app, app, nil)
			if err != nil {
				return run.fail(err)
			}
			id := in.Snapshot
			if snapshot != nil {
				id = snapshot.ID
			}
			sum, err := rs.Restore(ctx, id, app, mounts, includes...)
			if err != nil {
				return run.fail(err)
			}
			fmt.Fprintf(out, "%d volume(s) restored: %d files, %s\n", len(mounts), sum.FilesRestored, humanBytes(sum.BytesRestored))
			return nil
		},
	})
	add(kernel.Step{
		Name: "database", Change: "start the app's database and load the dump, when the snapshot holds one",
		Apply: func(ctx context.Context, out io.Writer) error {
			mf, err := loadStaged()
			if err != nil {
				return run.fail(err)
			}
			version := mf.PostgresVersion()
			if version == "" {
				fmt.Fprintln(out, "no database")
				return nil
			}
			dump := filepath.Join(staging, "postgres.dump")
			if _, err := os.Stat(dump); err != nil {
				return run.fail(errors.New("the snapshot holds no postgres.dump"))
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return run.fail(err)
			}
			defer e.Close()
			if err := e.EnsureNetwork(ctx, docker.AppNetwork(app)); err != nil {
				return run.fail(err)
			}
			// A new machine has none of the app's secrets: the ones the
			// manifest makes are made now, and the ones it can't make must
			// have been set first, as they are for a deploy.
			if err := ensureAppSecrets(r.Secrets, mf, out); err != nil {
				return run.fail(err)
			}
			initDir, err := restoreInit(staging, r.StateDir, app)
			if err != nil {
				return run.fail(err)
			}
			if err := ensurePostgres(ctx, e, r.Secrets, mf, initDir, dump, out); err != nil {
				return run.fail(err)
			}
			return nil
		},
	})
	add(kernel.Step{
		Name: "done", Change: "record the restore; the app is ready to deploy",
		Apply: func(ctx context.Context, out io.Writer) error {
			result := state.BackupRun{Detail: "data restored; deploy the app to run it"}
			if snapshot != nil {
				result.Snapshot, result.SnapshotAt = snapshot.ShortID, snapshot.Time
			} else {
				result.Snapshot = short(in.Snapshot)
			}
			if err := run.done(ctx, result); err != nil {
				return err
			}
			_ = os.RemoveAll(staging)
			fmt.Fprintf(out, "%s's data is on this machine; deploy it with quark deploy <its source directory>\n", app)
			return nil
		},
	})
	return plan, nil
}

// describeData says what an app keeps, for a restore's first line.
func describeData(m *manifest.Manifest) string {
	if m.Data == nil {
		return "no data"
	}
	var parts []string
	if v := m.PostgresVersion(); v != "" {
		parts = append(parts, "postgres "+v)
	}
	if n := len(m.DataVolumes()); n == 1 {
		parts = append(parts, "1 volume")
	} else if n > 1 {
		parts = append(parts, fmt.Sprintf("%d volumes", n))
	}
	if len(parts) == 0 {
		return "no data"
	}
	return joinWords(parts)
}

func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return parts[0] + " and " + parts[1]
	}
}

// initSnapshotDir is where a snapshot keeps the database's first-run
// scripts, which a restore on a new machine runs again before the dump.
const initSnapshotDir = "postgres-init"

// restoreInit puts the first-run scripts a snapshot holds where the
// database mounts them, and returns that directory, or "" without any.
func restoreInit(staging, stateDir, app string) (string, error) {
	from := filepath.Join(staging, initSnapshotDir)
	if !dirExists(from) {
		return "", nil
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return "", err
	}
	dir := InitDir(stateDir, app)
	if err := resetDir(dir); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}
