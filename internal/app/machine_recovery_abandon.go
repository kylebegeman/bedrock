package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

// RecoverAbandon releases a failed/private recovery while preserving its
// restored volumes, secret histories and operational history for inspection.
type RecoverAbandon struct{ Recovery RecoverMachine }

func (RecoverAbandon) Kind() string { return RecoverAbandonKind }

func (a RecoverAbandon) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in RecoverCutoverInput
	if json.Unmarshal(raw, &in) != nil || !recoveryIDPattern.MatchString(in.ID) {
		return nil, errors.New("abandon needs a valid recovery ID")
	}
	dir := RecoveryDirectory(a.Recovery.StateDir, in.ID)
	path := filepath.Join(dir, "recovery-record.json")
	if err := recoveryPlainFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var original machineRecoveryRecord
	if json.Unmarshal(data, &original) != nil {
		return nil, errors.New("invalid recovery record")
	}
	x := &machineRecovering{r: a.Recovery, in: original.Input, dir: dir}
	rec, err := x.load()
	if err != nil {
		return nil, err
	}
	if rec.Input.Rehearsal {
		return nil, errors.New("disposable rehearsals clean their own resources; retry the rehearsal instead")
	}
	if err := checkAbandonableRecovery(ctx, x, rec); err != nil {
		return nil, err
	}
	return &kernel.Plan{Target: "recovery " + in.ID, Recovery: kernel.Resume, Steps: []kernel.Step{
		{Name: "stop", Change: "remove only recovery-owned private containers and networks; preserve restored volumes and history", Apply: func(ctx context.Context, out io.Writer) error {
			rec, err := x.load()
			if err != nil {
				return err
			}
			if err := checkAbandonableRecovery(ctx, x, rec); err != nil {
				return err
			}
			if rec.ResourcesClaimed {
				e, err := docker.Connect(ctx)
				if err != nil {
					return err
				}
				defer e.Close()
				for _, rev := range rec.Catalog.Revisions {
					var m manifest.Manifest
					_ = json.Unmarshal(rev.Manifest, &m)
					n := x.names(&m)
					containers := make([]string, 0, len(n.containers)+2)
					for _, c := range n.containers {
						containers = append(containers, c)
					}
					if m.PostgresVersion() != "" {
						containers = append(containers, n.postgres)
					}
					if m.HasObjects() {
						containers = append(containers, n.objects)
					}
					for _, name := range containers {
						info, err := e.Inspect(ctx, name)
						if docker.IsNotFound(err) {
							continue
						}
						if err != nil {
							return err
						}
						if info.Labels["bedrock.recovery"] != in.ID {
							return fmt.Errorf("refusing to remove container %s because its recovery ownership changed", name)
						}
						if err := e.Remove(ctx, name, 5*time.Second); err != nil {
							return err
						}
					}
					if err := e.RemoveNetwork(ctx, n.network); err != nil {
						return err
					}
				}
			}
			fmt.Fprintln(out, "private recovery containers removed; restored data and history retained")
			return nil
		}},
		{Name: "release", Change: "remove the sealed source identity, record abandonment and release the machine reservation", Apply: func(ctx context.Context, out io.Writer) error {
			rec, err := x.load()
			if err != nil {
				return err
			}
			if err := checkAbandonableRecovery(ctx, x, rec); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(x.dir, "identity.age")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if rec.AbandonedAt.IsZero() {
				rec.AbandonedAt = time.Now().UTC()
				if err := x.save(rec); err != nil {
					return err
				}
			}
			if err := x.releaseReservation(); err != nil {
				return err
			}
			fmt.Fprintln(out, "recovery abandoned; machine reservation released")
			return nil
		}},
	}}, nil
}

func checkAbandonableRecovery(ctx context.Context, x *machineRecovering, rec *machineRecoveryRecord) error {
	if rec.Cutover {
		return errors.New("a live recovery cannot be abandoned")
	}
	if rec.CutoverStarted {
		return errors.New("cutover has already started; finish the same cutover instead of abandoning its runtime")
	}
	for _, ready := range rec.CutoverReady {
		if ready {
			return errors.New("cutover has already started; finish the same cutover instead of abandoning its runtime")
		}
	}
	active, err := x.r.Store.ActiveRevisions(ctx)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return errors.New("a recovery cannot be abandoned while target apps are active")
	}
	return nil
}
