package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/kylebegeman/quark/internal/cloudflare"
	"github.com/kylebegeman/quark/internal/dns"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/integration"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/state"
)

// DNSManager opens the Cloudflare integration for this machine, or says
// why it can't.
func DNSManager(sec *secrets.Store, addresses []string) (*dns.Manager, error) {
	cf, err := integration.LoadCloudflare(sec)
	if err != nil {
		return nil, err
	}
	client := cloudflare.New(cf.Token)
	if cf.API != "" {
		client.Base = cf.API
	}
	hostname, _ := os.Hostname()
	return &dns.Manager{CF: client, Hostname: hostname, Addresses: addresses}, nil
}

// errNoCloudflare says what to do when a manifest asks for records and
// the machine can't keep them.
func errNoCloudflare(app string, err error) error {
	return fmt.Errorf("%s's routes ask quark to keep their DNS records (dns: direct or proxied), but %w", app, err)
}

// managedHostsOf reads a revision's managed hosts.
func managedHostsOf(rev state.Revision) map[string]manifest.DNSMode {
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return nil
	}
	return m.ManagedHosts()
}

// keepRecords makes the records a manifest asks for and waits until every
// host points here. A record that points at another machine stops the
// deploy: moving a name is `quark dns point`, never a side effect.
func keepRecords(ctx context.Context, sec *secrets.Store, m *manifest.Manifest, addrs []string, out io.Writer) error {
	managed := m.ManagedHosts()
	if len(managed) > 0 {
		mgr, err := DNSManager(sec, addrs)
		if err != nil {
			return errNoCloudflare(m.App, err)
		}
		for _, host := range sortedKeys(managed) {
			outcome, err := mgr.Ensure(ctx, m.App, host, managed[host], false)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, outcome)
			if outcome.Proxied && outcome.Depth() > 1 {
				fmt.Fprintf(out, "note: Cloudflare's free certificate covers names one level below %s, so HTTPS to %s through the proxy fails unless the zone has Advanced Certificate Manager; dns: direct avoids it\n", outcome.Zone, host)
			}
		}
	}
	for _, host := range m.Hosts() {
		if managed[host] == manifest.DNSProxied {
			fmt.Fprintf(out, "%s is behind Cloudflare's proxy; its record names this machine\n", host)
			continue
		}
		if err := waitResolves(ctx, host, addrs, managed[host].Managed()); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s points here\n", host)
	}
	return nil
}

// waitResolves checks a host's public DNS, waiting up to two minutes for a
// record quark just made to reach the resolvers.
func waitResolves(ctx context.Context, host string, addrs []string, patient bool) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ok, pointsAt, err := edge.Resolves(ctx, host, addrs)
		if ok {
			return nil
		}
		if !patient || time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return errors.New(edge.DNSProblem(host, pointsAt, addrs))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// pruneRecords removes the records quark keeps for an app's hosts that
// its active revision no longer routes. It needs the integration only
// when there is something to remove.
func pruneRecords(ctx context.Context, store *state.Store, sec *secrets.Store, app string, addrs []string, out io.Writer) error {
	revs, err := store.Revisions(ctx, app)
	if err != nil {
		return err
	}
	keep := map[string]manifest.DNSMode{}
	dropped := map[string]bool{}
	for _, r := range revs {
		if r.Status == state.RevisionActive {
			keep = managedHostsOf(r)
		}
	}
	for _, r := range revs {
		if r.Status == state.RevisionActive {
			continue
		}
		for host := range managedHostsOf(r) {
			if _, still := keep[host]; !still {
				dropped[host] = true
			}
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	mgr, err := DNSManager(sec, addrs)
	if err != nil {
		fmt.Fprintf(out, "records for %v stay: %v\n", sortedKeys(dropped), err)
		return nil
	}
	removed, err := mgr.Remove(ctx, app, sortedKeys(dropped))
	if err != nil {
		return err
	}
	for _, host := range removed {
		fmt.Fprintf(out, "%s's record removed; %s no longer routes it\n", host, app)
	}
	return nil
}

// PointKind moves a host's record to this machine: the step of moving
// day when a name leaves one machine for another.
const PointKind = "dns.point"

// Point is the Definition for PointKind.
type Point struct {
	Store     *state.Store
	Secrets   *secrets.Store
	Addresses func(ctx context.Context) []string
}

// PointInput says which host, and whether it goes behind the proxy.
type PointInput struct {
	Host    string `json:"host"`
	Proxied bool   `json:"proxied,omitempty"`
}

// Kind implements kernel.Definition.
func (Point) Kind() string { return PointKind }

// Plan implements kernel.Definition.
func (p Point) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in PointInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("point input: %w", err)
	}
	// The app that routes the host: an active one, or else the newest
	// revision that asked for it, such as a first deploy that stopped
	// because the name still pointed at the old machine.
	app, mode, err := routedBy(ctx, p.Store, in.Host)
	if err != nil {
		return nil, err
	}
	if app == "" {
		return nil, fmt.Errorf("no app on this machine routes %s; deploy the app with it first, then point its name here", in.Host)
	}
	if in.Proxied {
		mode = manifest.DNSProxied
	}
	if _, err := integration.LoadCloudflare(p.Secrets); err != nil {
		return nil, fmt.Errorf("pointing a record needs the cloudflare integration: %w", err)
	}
	return &kernel.Plan{Target: in.Host, Recovery: kernel.Resume, Steps: []kernel.Step{{
		Name: "point", Change: fmt.Sprintf("make %s's record name this machine (%s), for %s", in.Host, mode, app),
		Apply: func(ctx context.Context, out io.Writer) error {
			addrs := p.Addresses(ctx)
			mgr, err := DNSManager(p.Secrets, addrs)
			if err != nil {
				return err
			}
			outcome, err := mgr.Ensure(ctx, app, in.Host, mode, true)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, outcome)
			if mode == manifest.DNSProxied {
				return nil
			}
			if err := waitResolves(ctx, in.Host, addrs, true); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s points here for the world\n", in.Host)
			return nil
		},
	}}}, nil
}

// Routes lists every host the active revisions route, with its mode.
func Routes(ctx context.Context, store *state.Store) ([]dns.Route, error) {
	active, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	var out []dns.Route
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			continue
		}
		managed := m.ManagedHosts()
		for _, h := range m.Hosts() {
			out = append(out, dns.Route{App: rev.App, Host: h, Mode: managed[h]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// routedBy finds the app whose revisions route a host, preferring an
// active revision, and the record mode it asks for.
func routedBy(ctx context.Context, store *state.Store, host string) (string, manifest.DNSMode, error) {
	apps, err := store.AppNames(ctx)
	if err != nil {
		return "", "", err
	}
	type found struct {
		app    string
		mode   manifest.DNSMode
		active bool
		at     time.Time
	}
	var best *found
	for _, name := range apps {
		revs, err := store.Revisions(ctx, name)
		if err != nil {
			return "", "", err
		}
		for _, rev := range revs {
			var m manifest.Manifest
			if err := json.Unmarshal(rev.Manifest, &m); err != nil {
				continue
			}
			for _, h := range m.Hosts() {
				if h != host {
					continue
				}
				mode := m.ManagedHosts()[h]
				if mode == manifest.DNSManual {
					mode = manifest.DNSDirect
				}
				f := found{rev.App, mode, rev.Status == state.RevisionActive, rev.CreatedAt}
				if best == nil || (f.active && !best.active) || (f.active == best.active && f.at.After(best.at)) {
					best = &f
				}
			}
		}
	}
	if best == nil {
		return "", "", nil
	}
	return best.app, best.mode, nil
}
