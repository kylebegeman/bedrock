package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/edge"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// RemoveKind takes an app off the machine: its routes, containers, images
// and network. Its data stays unless asked for.
const RemoveKind = "app.remove"

// Remove is the Definition for RemoveKind.
type Remove struct {
	Store     *state.Store
	Secrets   *secrets.Store
	Addresses func(ctx context.Context) []string
	// StateDir is bedrock's state directory; empty means /var/lib/bedrock.
	StateDir string
}

// RemoveInput says which app, and whether its data goes too.
type RemoveInput struct {
	App string `json:"app"`
	// Data also removes the app's volumes and database. There is no way
	// back from that except a backup.
	Data bool `json:"data,omitempty"`
}

// Kind implements kernel.Definition.
func (Remove) Kind() string { return RemoveKind }

// Plan implements kernel.Definition.
func (r Remove) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in RemoveInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("remove input: %w", err)
	}
	if in.App == "" {
		return nil, errors.New("which app?")
	}
	revs, err := r.Store.Revisions(ctx, in.App)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, fmt.Errorf("%s isn't on this machine", in.App)
	}
	var m manifest.Manifest
	for _, rev := range revs {
		if rev.Status == state.RevisionActive {
			_ = json.Unmarshal(rev.Manifest, &m)
		}
	}
	if m.App == "" {
		_ = json.Unmarshal(revs[0].Manifest, &m)
	}
	store := r.Store
	plan := &kernel.Plan{Target: in.App, Recovery: kernel.Resume}
	plan.Steps = append(plan.Steps,
		kernel.Step{
			Name: "unroute", Change: "take the app's routes off the edge",
			Apply: func(ctx context.Context, out io.Writer) error {
				for _, rev := range revs {
					if rev.Status == state.RevisionActive || rev.Status == state.RevisionPrevious {
						if err := store.SetRevisionStatus(ctx, in.App, rev.ID, state.RevisionRetired); err != nil {
							return err
						}
					}
				}
				cfg, err := edgeConfig(ctx, store, r.Secrets)
				if err != nil {
					return err
				}
				admin := edge.NewAdmin()
				if admin.Answers(ctx) {
					if err := admin.Load(ctx, cfg); err != nil {
						return err
					}
				}
				fmt.Fprintln(out, "routes removed")
				return nil
			},
		},
		kernel.Step{
			Name: "dns", Change: "remove the DNS records bedrock keeps for the app" + recordsNote(m),
			Apply: func(ctx context.Context, out io.Writer) error {
				managed := m.ManagedHosts()
				if len(managed) == 0 {
					fmt.Fprintln(out, "the app keeps no records")
					return nil
				}
				var addrs []string
				if r.Addresses != nil {
					addrs = r.Addresses(ctx)
				}
				mgr, err := DNSManager(r.Secrets, addrs)
				if err != nil {
					fmt.Fprintf(out, "records for %v stay: %v\n", sortedKeys(managed), err)
					return nil
				}
				removed, err := mgr.Remove(ctx, in.App, sortedKeys(managed))
				if err != nil {
					return err
				}
				for _, host := range removed {
					fmt.Fprintf(out, "%s's record removed\n", host)
				}
				return nil
			},
		},
		kernel.Step{
			Name: "containers", Change: "stop and remove the app's containers and images",
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := docker.Connect(ctx)
				if err != nil {
					return err
				}
				defer e.Close()
				owned, err := e.Owned(ctx)
				if err != nil {
					return err
				}
				for _, c := range owned {
					if c.Labels[docker.LabelApp] != in.App {
						continue
					}
					workload := c.Labels[docker.LabelWorkload]
					if workload == postgresWorkload || workload == objectsWorkload {
						// The data services get time to write everything out.
						if err := e.Remove(ctx, c.Name, 60*time.Second); err != nil {
							return err
						}
						if !in.Data {
							fmt.Fprintf(out, "%s stopped; its volume stays\n", c.Name)
						} else {
							fmt.Fprintf(out, "%s stopped\n", c.Name)
						}
						continue
					}
					grace := 10 * time.Second
					if w, ok := m.Workloads[workload]; ok {
						grace = w.GraceOr(grace)
					}
					if err := e.Remove(ctx, c.Name, grace); err != nil {
						return err
					}
					fmt.Fprintf(out, "%s removed\n", c.Name)
				}
				images, err := e.OwnedImages(ctx)
				if err != nil {
					return err
				}
				for _, img := range images {
					if img.Labels[docker.LabelApp] == in.App {
						_ = e.RemoveImage(ctx, img.ID)
					}
				}
				return nil
			},
		},
	)
	if in.Data {
		plan.Steps = append(plan.Steps, kernel.Step{
			Name: "data", Change: "remove the app's volumes, database and object store (no way back except a backup)",
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := docker.Connect(ctx)
				if err != nil {
					return err
				}
				defer e.Close()
				names := []string{docker.VolumeName(in.App, postgresWorkload)}
				for _, v := range m.DataVolumes() {
					names = append(names, docker.VolumeName(in.App, v))
				}
				for _, v := range names {
					removed, err := e.RemoveVolume(ctx, v)
					if err != nil {
						return err
					}
					if removed {
						fmt.Fprintf(out, "volume %s removed\n", v)
					}
				}
				// The copy of the database's first-run scripts goes too.
				stateDir := r.StateDir
				if stateDir == "" {
					stateDir = defaultStateDir
				}
				if err := os.RemoveAll(filepath.Dir(InitDir(stateDir, in.App))); err != nil {
					return err
				}
				return nil
			},
		})
	}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "forget", Change: "remove the app's network and forget it",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer e.Close()
			if err := edge.Leave(ctx, e, in.App); err != nil {
				return err
			}
			if err := e.RemoveNetwork(ctx, docker.AppNetwork(in.App)); err != nil {
				return err
			}
			if err := store.RemoveApp(ctx, in.App); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s forgotten\n", in.App)
			return nil
		},
	})
	return plan, nil
}

func recordsNote(m manifest.Manifest) string {
	if managed := m.ManagedHosts(); len(managed) > 0 {
		return ": " + strings.Join(sortedKeys(managed), ", ")
	}
	return " (none)"
}
