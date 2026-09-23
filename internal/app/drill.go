package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/restic"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// DrillKind proves a backup: it restores the latest snapshot beside the
// app, into scratch volumes and a scratch database, starts the app's
// long-running workloads on them, checks what came back, and cleans up.
const DrillKind = "app.drill"

// Drill is the Definition for DrillKind.
type Drill struct {
	Store    *state.Store
	Secrets  *secrets.Store
	StateDir string
}

// DrillInput says which app and which snapshot; empty means the latest.
type DrillInput struct {
	App      string `json:"app"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Kind implements kernel.Definition.
func (Drill) Kind() string { return DrillKind }

// drillNames are the scratch things a drill makes.
type drillNames struct {
	dir, network, postgres, postgresVolume, objects string
	volumes                                         map[string]string // app volume name -> drill volume
	containers                                      map[string]string // workload -> drill container
}

func newDrillNames(stateDir string, m *manifest.Manifest) drillNames {
	app := m.App
	// A dot can't appear in an app's name, so these never collide with
	// another app's volumes, which the cleanup would otherwise remove.
	prefix := "bedrock-" + app + ".drill"
	n := drillNames{
		dir:            filepath.Join(stateDir, "drills", app),
		network:        prefix,
		postgres:       prefix + ".postgres",
		postgresVolume: prefix + ".postgres",
		objects:        prefix + ".objects",
		volumes:        map[string]string{},
		containers:     map[string]string{},
	}
	for _, v := range m.DataVolumes() {
		n.volumes[v] = prefix + "." + v
	}
	for _, w := range m.WorkloadNames() {
		if m.Workloads[w].LongRunning() {
			n.containers[w] = prefix + "." + w
		}
	}
	return n
}

// cleanup removes everything a drill made. It never fails loudly: what
// it can't remove, the next drill removes first.
func (n drillNames) cleanup(e *docker.Engine) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, c := range n.containers {
		_ = e.Remove(ctx, c, 5*time.Second)
	}
	_ = e.Remove(ctx, n.postgres, 10*time.Second)
	_ = e.Remove(ctx, n.objects, 10*time.Second)
	_ = e.RemoveNetwork(ctx, n.network)
	_, _ = e.RemoveVolume(ctx, n.postgresVolume)
	for _, v := range n.volumes {
		_, _ = e.RemoveVolume(ctx, v)
	}
	_ = os.RemoveAll(n.dir)
}

// drillState is what the steps share.
type drillState struct {
	run      *backupRun
	names    drillNames
	snapshot *restic.Snapshot
	restored *restic.RestoreSummary
	details  []string
	started  time.Time
}

func (d *drillState) abort(e *docker.Engine, err error) error {
	d.names.cleanup(e)
	return d.run.fail(err)
}

// Plan implements kernel.Definition.
func (dr Drill) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in DrillInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("drill input: %w", err)
	}
	if in.App == "" {
		return nil, errors.New("which app?")
	}
	if in.Snapshot == "" {
		in.Snapshot = "latest"
	}
	rev, err := dr.Store.RevisionWithStatus(ctx, in.App, state.RevisionActive)
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
		return nil, fmt.Errorf("%s keeps no data; there is nothing to drill", in.App)
	}
	for name, workload := range m.Workloads {
		if workload.LongRunning() && workload.Privileged {
			return nil, fmt.Errorf("cannot safely drill privileged workload %s: it could escape the scratch network", name)
		}
	}
	app := in.App
	d := &drillState{run: &backupRun{store: dr.Store, app: app, kind: state.BackupRunDrill}, names: newDrillNames(dr.StateDir, &m)}
	verifySQL, atLeast := m.VerifyQuery()
	plan := &kernel.Plan{Target: app, Recovery: kernel.Resume}
	add := func(st kernel.Step) { plan.Steps = append(plan.Steps, st) }

	add(kernel.Step{
		Name: "restore", Change: fmt.Sprintf("restore snapshot %s into scratch volumes beside the app", in.Snapshot),
		Apply: func(ctx context.Context, out io.Writer) error {
			d.started = time.Now().UTC()
			if err := d.run.begin(ctx); err != nil {
				return err
			}
			e, err := docker.Connect(ctx)
			if err != nil {
				return d.run.fail(err)
			}
			defer e.Close()
			d.names.cleanup(e)
			if err := os.MkdirAll(d.names.dir, 0o700); err != nil {
				return d.abort(e, err)
			}
			r, _, err := openRepo(dr.Secrets, e, app, app, nil)
			if err != nil {
				return d.abort(e, err)
			}
			if in.Snapshot == "latest" {
				d.snapshot, err = r.Latest(ctx, app)
				if errors.Is(err, restic.ErrNoRepository) {
					return d.abort(e, fmt.Errorf("%s has no backups yet; run bedrock backup %s first", app, app))
				}
				if err != nil {
					return d.abort(e, err)
				}
			} else {
				snaps, err := r.Snapshots(ctx, app)
				if err != nil {
					return d.abort(e, err)
				}
				for i := range snaps {
					if strings.HasPrefix(snaps[i].ID, in.Snapshot) {
						d.snapshot = &snaps[i]
					}
				}
				if d.snapshot == nil {
					return d.abort(e, fmt.Errorf("no snapshot %s for %s", in.Snapshot, app))
				}
			}
			mounts := []string{restic.DirMount(d.names.dir, false)}
			for _, v := range sortedKeys(d.names.volumes) {
				if err := os.MkdirAll(filepath.Join(d.names.dir, "volumes", v), 0o700); err != nil {
					return d.abort(e, err)
				}
				if err := e.EnsureVolume(ctx, d.names.volumes[v]); err != nil {
					return d.abort(e, err)
				}
				mounts = append(mounts, restic.VolumeMount(d.names.volumes[v], v, false))
			}
			d.restored, err = r.Restore(ctx, d.snapshot.ID, app, mounts)
			if err != nil {
				return d.abort(e, err)
			}
			if d.restored.FilesRestored != d.restored.TotalFiles {
				return d.abort(e, fmt.Errorf("restored %d of %d files", d.restored.FilesRestored, d.restored.TotalFiles))
			}
			d.details = append(d.details, fmt.Sprintf("%d files (%s) restored", d.restored.FilesRestored, humanBytes(d.restored.BytesRestored)))
			fmt.Fprintf(out, "snapshot %s from %s: %s\n", d.snapshot.ShortID, d.snapshot.Time.Local().Format("2006-01-02 15:04"), d.details[0])
			return nil
		},
	})
	// The scratch services answer, on the drill's own network, to the
	// names the app's services have on the app's: the app's secrets,
	// DATABASE_URL and every derived URL included, work unchanged against
	// the restored copies, and can't reach the real ones.
	if m.PostgresVersion() != "" {
		change := "start a scratch database and load the dump"
		if verifySQL != "" {
			change += ", then run the verify query"
		}
		add(kernel.Step{
			Name: "database", Change: change,
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := docker.Connect(ctx)
				if err != nil {
					return d.run.fail(err)
				}
				defer e.Close()
				if err := e.EnsureInternalNetwork(ctx, d.names.network); err != nil {
					return d.abort(e, err)
				}
				values, err := dr.Secrets.Load(app, rev.SecretsVersion)
				if err != nil {
					return d.abort(e, err)
				}
				// The first-run scripts make the roles the dump names.
				svc, err := postgresService(&m, values, filepath.Join(d.names.dir, initSnapshotDir))
				if err != nil {
					return d.abort(e, err)
				}
				if !keepsOwners(&m) {
					svc.InitDir = ""
				} else if !dirExists(svc.InitDir) {
					return d.abort(e, errors.New("the snapshot holds no database init scripts"))
				}
				svc.Container, svc.Volume, svc.Network = d.names.postgres, d.names.postgresVolume, d.names.network
				svc.Labels = map[string]string{docker.LabelApp: app, docker.LabelWorkload: "drill"}
				svc.Aliases = []string{PostgresContainer(app)}
				if err := svc.start(ctx, e, out); err != nil {
					return d.abort(e, err)
				}
				dump := filepath.Join(d.names.dir, "postgres.dump")
				if _, err := os.Stat(dump); err != nil {
					return d.abort(e, errors.New("the snapshot holds no postgres.dump"))
				}
				tables, err := restoreDump(ctx, e, svc, dump, keepsOwners(&m))
				if err != nil {
					return d.abort(e, err)
				}
				if n, _ := strconv.Atoi(tables); n == 0 {
					return d.abort(e, errors.New("the restored database has no tables"))
				}
				d.details = append(d.details, tables+" tables")
				fmt.Fprintf(out, "database restored: %s tables\n", tables)
				if verifySQL != "" {
					got, err := scalar(ctx, e, svc, verifySQL)
					if err != nil {
						return d.abort(e, fmt.Errorf("verify query: %w", err))
					}
					n, err := strconv.Atoi(got)
					if err != nil {
						return d.abort(e, fmt.Errorf("the verify query returned %q, not a number", got))
					}
					if n < atLeast {
						return d.abort(e, fmt.Errorf("the verify query returned %d, below %d", n, atLeast))
					}
					d.details = append(d.details, fmt.Sprintf("verify query %d (at least %d)", n, atLeast))
					fmt.Fprintf(out, "verify query: %d\n", n)
				}
				return nil
			},
		})
	}
	if m.HasObjects() {
		add(kernel.Step{
			Name: "objects", Change: "start a scratch object store on the restored files",
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := docker.Connect(ctx)
				if err != nil {
					return d.run.fail(err)
				}
				defer e.Close()
				if err := e.EnsureInternalNetwork(ctx, d.names.network); err != nil {
					return d.abort(e, err)
				}
				values, err := dr.Secrets.Load(app, rev.SecretsVersion)
				if err != nil {
					return d.abort(e, err)
				}
				o := objectsServiceFor(&m, values)
				o.Container, o.Volume, o.Network = d.names.objects, d.names.volumes[manifest.ObjectsVolume], d.names.network
				if empty, err := e.VolumeEmpty(ctx, o.Volume); err != nil {
					return d.abort(e, err)
				} else if empty {
					return d.abort(e, errors.New("the snapshot holds no object store files"))
				}
				o.Labels = map[string]string{docker.LabelApp: app, docker.LabelWorkload: "drill"}
				o.Aliases = []string{ObjectsContainer(app)}
				if err := o.start(ctx, e, out); err != nil {
					return d.abort(e, err)
				}
				d.details = append(d.details, "object store answered")
				fmt.Fprintln(out, "object store answered on the restored files")
				return nil
			},
		})
	}
	if len(d.names.containers) > 0 {
		add(kernel.Step{
			Name: "app", Change: "start the app's long-running workloads on the restored data and wait for them",
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := docker.Connect(ctx)
				if err != nil {
					return d.run.fail(err)
				}
				defer e.Close()
				if err := e.EnsureInternalNetwork(ctx, d.names.network); err != nil {
					return d.abort(e, err)
				}
				values, err := dr.Secrets.Load(app, rev.SecretsVersion)
				if err != nil {
					return d.abort(e, err)
				}
				for _, name := range sortedKeys(d.names.containers) {
					w := m.Workloads[name]
					image := rev.Images[name]
					if image == "" {
						return d.abort(e, fmt.Errorf("no image recorded for %s", name))
					}
					spec, err := drillSpec(&m, rev.ID, name, image, values, d.names)
					if err != nil {
						return d.abort(e, err)
					}
					if _, err := isolate(ctx, e, &spec, w, image); err != nil {
						return d.abort(e, err)
					}
					if err := e.Run(ctx, spec); err != nil {
						return d.abort(e, err)
					}
				}
				// Dependencies must all be running before any readiness probe.
				for _, name := range sortedKeys(d.names.containers) {
					w := m.Workloads[name]
					started := time.Now()
					if err := waitReady(ctx, e, d.names.containers[name], w); err != nil {
						return d.abort(e, fmt.Errorf("%s on the restored data: %w", name, err))
					}
					took := time.Since(started).Round(100 * time.Millisecond)
					d.details = append(d.details, fmt.Sprintf("%s answered in %s", name, took))
					fmt.Fprintf(out, "%s answered in %s\n", name, took)
				}
				return nil
			},
		})
	}
	add(kernel.Step{
		Name: "cleanup", Change: "remove the scratch volumes, database and containers, and record the drill",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return d.run.fail(err)
			}
			defer e.Close()
			d.names.cleanup(e)
			if d.snapshot == nil {
				// Resumed past the restore step: the record can't say more.
				return d.run.fail(errors.New("the drill was interrupted; run it again"))
			}
			point := time.Since(d.snapshot.Time).Round(time.Minute)
			d.details = append(d.details, fmt.Sprintf("recovery point %s before the drill, recovery time %s", point, time.Since(d.started).Round(time.Second)))
			result := state.BackupRun{Snapshot: d.snapshot.ShortID, SnapshotAt: d.snapshot.Time, Detail: strings.Join(d.details, "; ")}
			if d.restored != nil {
				result.Files, result.Bytes = d.restored.FilesRestored, d.restored.BytesRestored
			}
			if err := d.run.done(ctx, result); err != nil {
				return err
			}
			fmt.Fprintln(out, result.Detail)
			return nil
		},
	})
	return plan, nil
}

// drillSpec is a workload's container started on the restored data: on
// the drill's own network and volumes, and published nowhere on the
// machine, where the app's running containers already hold its ports.
func drillSpec(m *manifest.Manifest, revision, name, image string, values map[string]string, names drillNames) (docker.Spec, error) {
	w := m.Workloads[name]
	spec, err := containerSpec(m, name, w, revision, image, values)
	if err != nil {
		return spec, err
	}
	spec.Name = names.containers[name]
	spec.Networks = []string{names.network}
	spec.Restart = false
	spec.Publish = nil
	spec.Mounts = nil
	for _, mt := range w.Mounts {
		spec.Mounts = append(spec.Mounts, names.volumes[mt.Volume]+":"+mt.Path)
	}
	return spec, nil
}
