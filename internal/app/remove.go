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
	"github.com/kylebegeman/bedrock/internal/integration"
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
	// Data also removes the app's volumes, database and sealed secrets.
	// There is no way back from that except a backup. For an app that is
	// no longer on the machine, it removes whatever of it is left.
	Data bool `json:"data,omitempty"`
	// Secrets forgets the app's sealed secrets while its data stays.
	// Without it or Data they stay, so the app can be restored or deployed
	// again with what it had.
	Secrets bool `json:"secrets,omitempty"`
}

// Kind implements kernel.Definition.
func (Remove) Kind() string { return RemoveKind }

// removal is one app being taken off the machine: what its steps share,
// and each step as a method.
type removal struct {
	r        Remove
	in       RemoveInput
	revs     []state.Revision
	m        manifest.Manifest
	stateDir string
	// secrets says whether the app's sealed secrets go too.
	secrets bool
}

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
	stateDir := r.StateDir
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	if len(revs) == 0 {
		// Removed before, without its data: what it left can still go.
		return r.planLeftovers(ctx, in, stateDir)
	}
	// The active revision's manifest says what the app keeps; failing
	// that, the newest one's. One that doesn't parse would leave records
	// and volumes behind unseen, so it stops the remove instead.
	described := revs[0]
	for _, rev := range revs {
		if rev.Status == state.RevisionActive {
			described = rev
		}
	}
	x := &removal{r: r, in: in, revs: revs, stateDir: stateDir}
	if err := json.Unmarshal(described.Manifest, &x.m); err != nil {
		return nil, fmt.Errorf("revision %s of %s: its manifest doesn't parse: %w", described.ID, in.App, err)
	}
	// With its data gone, the secrets that opened it go too: left behind,
	// they are keys to nothing on this machine that nothing removes.
	x.secrets = in.Secrets || in.Data
	plan := &kernel.Plan{Target: in.App, Recovery: kernel.Resume}
	plan.Steps = append(plan.Steps, x.unrouteStep(), x.dnsStep(), x.containersStep())
	if in.Data {
		plan.Steps = append(plan.Steps, x.dataStep())
	}
	plan.Steps = append(plan.Steps, x.forgetStep())
	return plan, nil
}

func (x *removal) unrouteStep() kernel.Step {
	return kernel.Step{
		Name: "unroute", Change: "take the app's routes off the edge",
		Apply: func(ctx context.Context, out io.Writer) error {
			for _, rev := range x.revs {
				if rev.Status == state.RevisionActive || rev.Status == state.RevisionPrevious {
					if err := x.r.Store.SetRevisionStatus(ctx, x.in.App, rev.ID, state.RevisionRetired); err != nil {
						return err
					}
				}
			}
			cfg, err := edgeConfig(ctx, x.r.Store, x.r.Secrets)
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
	}
}

func (x *removal) dnsStep() kernel.Step {
	return kernel.Step{
		Name: "dns", Change: "remove the DNS records bedrock keeps for the app" + recordsNote(x.m),
		Apply: func(ctx context.Context, out io.Writer) error {
			managed := x.m.ManagedHosts()
			if len(managed) == 0 {
				fmt.Fprintln(out, "the app keeps no records")
				return nil
			}
			var addrs []string
			if x.r.Addresses != nil {
				addrs = x.r.Addresses(ctx)
			}
			mgr, err := DNSManager(x.r.Secrets, addrs)
			if err != nil {
				fmt.Fprintf(out, "records for %v stay: %v\n", sortedKeys(managed), err)
				return nil
			}
			removed, err := mgr.Remove(ctx, x.in.App, sortedKeys(managed))
			if err != nil {
				return err
			}
			for _, host := range removed {
				fmt.Fprintf(out, "%s's record removed\n", host)
			}
			return nil
		},
	}
}

func (x *removal) containersStep() kernel.Step {
	return kernel.Step{
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
				if c.Labels[docker.LabelApp] == x.in.App {
					if err := x.removeContainer(ctx, e, c, out); err != nil {
						return err
					}
				}
			}
			images, err := e.OwnedImages(ctx)
			if err != nil {
				return err
			}
			for _, img := range images {
				if img.Labels[docker.LabelApp] == x.in.App {
					if err := e.RemoveImage(ctx, img.ID); err != nil {
						fmt.Fprintf(out, "image %s stays: %v\n", img.Describe(), err)
					}
				}
			}
			return nil
		},
	}
}

// removeContainer stops and removes one of the app's containers, giving
// its data services a minute to write everything out and a workload the
// grace its manifest asks for.
func (x *removal) removeContainer(ctx context.Context, e *docker.Engine, c docker.Info, out io.Writer) error {
	workload := c.Labels[docker.LabelWorkload]
	if workload == postgresWorkload || workload == objectsWorkload {
		if err := e.Remove(ctx, c.Name, 60*time.Second); err != nil {
			return err
		}
		if !x.in.Data {
			fmt.Fprintf(out, "%s stopped; its volume stays\n", c.Name)
		} else {
			fmt.Fprintf(out, "%s stopped\n", c.Name)
		}
		return nil
	}
	grace := 10 * time.Second
	if w, ok := x.m.Workloads[workload]; ok {
		grace = w.GraceOr(grace)
	}
	if err := e.Remove(ctx, c.Name, grace); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s removed\n", c.Name)
	return nil
}

func (x *removal) dataStep() kernel.Step {
	return kernel.Step{
		Name: "data", Change: "remove the app's volumes, database and object store (no way back except a backup; its backups in the storage bucket stay)",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer e.Close()
			names := []string{docker.VolumeName(x.in.App, postgresWorkload)}
			for _, v := range x.m.DataVolumes() {
				names = append(names, docker.VolumeName(x.in.App, v))
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
			return os.RemoveAll(filepath.Dir(InitDir(x.stateDir, x.in.App)))
		},
	}
}

func (x *removal) forgetStep() kernel.Step {
	change := "remove the app's network and forget it"
	switch {
	case x.secrets && x.m.Preview != nil:
		change += ", and the secrets it was given as a preview"
	case x.secrets:
		change += ", and its sealed secrets (only a machine backup made before now, bedrock backup bedrock, keeps them)"
	}
	return kernel.Step{
		Name: "forget", Change: change,
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer e.Close()
			if err := edge.Leave(ctx, e, x.in.App); err != nil {
				return err
			}
			if err := e.RemoveNetwork(ctx, docker.AppNetwork(x.in.App)); err != nil {
				return err
			}
			if err := forgetApp(ctx, x.r.Store, x.r.Secrets, x.stateDir, x.in.App, x.secrets, out); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s forgotten\n", x.in.App)
			return nil
		},
	}
}

// forgetApp drops what the machine keeps about an app beside its
// containers: its rows, its staging directories, the webhook credentials
// bedrock made for it and, when asked, its sealed secrets. Its backup runs
// stay, because they say where its data went.
func forgetApp(ctx context.Context, store *state.Store, sec *secrets.Store, stateDir, app string, secretsToo bool, out io.Writer) error {
	if err := store.RemoveApp(ctx, app); err != nil {
		return err
	}
	for _, dir := range []string{"backups", "restore", "drills", "builds"} {
		if err := os.RemoveAll(filepath.Join(stateDir, dir, app)); err != nil {
			return err
		}
	}
	if sec == nil {
		return nil
	}
	own, _, err := sec.LoadCurrent(integration.App)
	if err != nil {
		return err
	}
	changes := map[string]*string{}
	for _, name := range []string{integration.WebhookSecretName(app), integration.DeployKeyName(app)} {
		if _, ok := own[name]; ok {
			changes[name] = nil
		}
	}
	if len(changes) > 0 {
		if _, err := sec.SetAll(integration.App, changes); err != nil {
			return err
		}
		fmt.Fprintln(out, "the webhook secret and deploy key made for it are gone")
	}
	if secretsToo {
		if err := sec.RemoveApp(app); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s's secrets forgotten\n", app)
	}
	return nil
}

func recordsNote(m manifest.Manifest) string {
	if managed := m.ManagedHosts(); len(managed) > 0 {
		return ": " + strings.Join(sortedKeys(managed), ", ")
	}
	return " (none)"
}
