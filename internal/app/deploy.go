// Package app deploys and rolls back apps: builds their images, starts a
// new revision beside the old, waits for it to be ready, switches the
// edge, checks it, and retires what it replaced.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/integration"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/state"
)

// DeployKind deploys a source directory as a new revision of its app.
const DeployKind = "app.deploy"

// RollbackKind makes an earlier revision active again.
const RollbackKind = "app.rollback"

// Deploy is the Definition for DeployKind.
type Deploy struct {
	Store   *state.Store
	Secrets *secrets.Store
	// Addresses are this machine's public addresses, for the DNS check.
	Addresses func(ctx context.Context) []string
}

// DeployInput says what to deploy.
type DeployInput struct {
	// Source is the directory holding quark.yaml and the code.
	Source string `json:"source"`
	// Revision names the new revision; the CLI makes one from the clock.
	Revision string `json:"revision"`
	// Restore loads data before the first start.
	Restore Restore `json:"restore,omitempty"`
	// Commit is the source's commit when the caller knows it, as a push
	// does; otherwise it is read from the source when it is a checkout.
	Commit string `json:"commit,omitempty"`
}

var revisionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{3,40}$`)

// Kind implements kernel.Definition.
func (Deploy) Kind() string { return DeployKind }

// Plan implements kernel.Definition.
func (d Deploy) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in DeployInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("deploy input: %w", err)
	}
	if !filepath.IsAbs(in.Source) {
		return nil, errors.New("deploy needs the absolute path of the source directory")
	}
	if !revisionPattern.MatchString(in.Revision) {
		return nil, fmt.Errorf("revision %q must be lowercase letters, digits and hyphens", in.Revision)
	}
	// The manifest is read as root: it must be a file of the source's own,
	// never a link out of it.
	if info, err := os.Lstat(filepath.Join(in.Source, manifest.FileName)); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a plain file, not a link", manifest.FileName)
	}
	m, err := manifest.Load(in.Source)
	if err != nil {
		return nil, err
	}
	if len(m.ManagedHosts()) > 0 {
		if _, err := integration.LoadCloudflare(d.Secrets); err != nil {
			return nil, errNoCloudflare(m.App, err)
		}
	}
	commit := in.Commit
	if commit == "" {
		commit = sourceCommit(ctx, in.Source)
	}
	return d.rollout(ctx, m, in.Revision, &buildFrom{source: in.Source, commit: commit, restore: in.Restore})
}

// buildFrom says images come from a source directory; nil means they are
// already recorded (a rollback).
type buildFrom struct {
	source  string
	commit  string
	restore Restore
}

// rollout is the plan both deploy and rollback share.
func (d Deploy) rollout(ctx context.Context, m *manifest.Manifest, revision string, from *buildFrom) (*kernel.Plan, error) {
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	store := d.Store
	images := map[string]string{}
	if from == nil {
		rev, err := store.GetRevision(ctx, m.App, revision)
		if err != nil {
			return nil, fmt.Errorf("revision %s of %s: %w", revision, m.App, err)
		}
		images = rev.Images
	}
	containers := map[string]string{}
	for _, name := range m.WorkloadNames() {
		if m.Workloads[name].LongRunning() {
			containers[name] = docker.ContainerName(m.App, name, revision)
		}
	}
	hosts := m.Hosts()
	target := fmt.Sprintf("%s %s", m.App, revision)
	plan := &kernel.Plan{Target: target, Recovery: kernel.Resume}
	add := func(st kernel.Step) { plan.Steps = append(plan.Steps, st) }

	var engine *docker.Engine
	connect := func(ctx context.Context) (*docker.Engine, error) {
		if engine != nil {
			return engine, nil
		}
		e, err := docker.Connect(ctx)
		if err != nil {
			return nil, err
		}
		engine = e
		return e, nil
	}

	if from != nil {
		add(kernel.Step{
			Name: "record", Change: fmt.Sprintf("record revision %s of %s", revision, m.App),
			Note: noteCommit(from.commit),
			Apply: func(ctx context.Context, out io.Writer) error {
				return store.SaveRevision(ctx, state.Revision{App: m.App, ID: revision, Status: state.RevisionFailed, Manifest: manifestJSON, Images: images, Containers: containers, Source: from.commit, CreatedAt: time.Now().UTC()})
			},
		})
	}
	add(kernel.Step{
		Name: "edge", Change: "run the edge, the app's own network and the one it shares with the edge alone",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			boot, err := EdgeConfig(ctx, store)
			if err != nil {
				return err
			}
			if err := edge.Ensure(ctx, e, boot, out); err != nil {
				return err
			}
			if err := e.EnsureNetwork(ctx, docker.AppNetwork(m.App)); err != nil {
				return err
			}
			if servesAny(m) {
				return edge.Join(ctx, e, m.App)
			}
			return nil
		},
	})
	// Names first: a record that points at another machine stops the
	// deploy before it costs a build.
	if len(hosts) > 0 {
		change := "confirm " + strings.Join(hosts, ", ") + " point at this machine"
		if managed := m.ManagedHosts(); len(managed) > 0 {
			change = "keep the DNS records for " + strings.Join(sortedKeys(managed), ", ") + " and confirm every host points here"
		}
		add(kernel.Step{
			Name: "dns", Change: change,
			Apply: func(ctx context.Context, out io.Writer) error {
				return keepRecords(ctx, d.Secrets, m, d.Addresses(ctx), out)
			},
		})
	}
	if m.Data != nil {
		var restore Restore
		if from != nil {
			restore = from.restore
		}
		add(kernel.Step{
			Name: "data", Change: "keep the app's database and volumes" + dataSummary(m),
			Note: restoreNote(restore),
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := connect(ctx)
				if err != nil {
					return err
				}
				return ensureData(ctx, e, d.Secrets, m, restore, out)
			},
		})
	}
	if from != nil {
		for _, name := range m.WorkloadNames() {
			w := m.Workloads[name]
			ref := docker.ImageRef(m.App, name, revision)
			switch {
			case w.Kind == manifest.Static:
				add(kernel.Step{
					Name: "build-" + name, Change: fmt.Sprintf("build %s from %s (static files)", name, w.Dir),
					Apply: func(ctx context.Context, out io.Writer) error {
						context, err := docker.StaticContext(from.source, w.Dir)
						if err != nil {
							return err
						}
						defer os.RemoveAll(context)
						if err := docker.Build(ctx, docker.BuildSpec{Ref: ref, Context: context, Labels: imageLabels(m.App, name, revision)}, filtered(out)); err != nil {
							return err
						}
						return recordImage(ctx, store, connect, m.App, revision, name, ref, images, out)
					},
				})
			case w.Build != nil:
				add(kernel.Step{
					Name: "build-" + name, Change: fmt.Sprintf("build %s from %s", name, orDot(w.Build.Context)),
					Apply: func(ctx context.Context, out io.Writer) error {
						spec := docker.BuildSpec{Ref: ref, Context: filepath.Join(from.source, orDot(w.Build.Context)), Dockerfile: w.Build.Dockerfile, Target: w.Build.Target, Args: w.Build.Args, Labels: imageLabels(m.App, name, revision)}
						if err := docker.Build(ctx, spec, filtered(out)); err != nil {
							return err
						}
						return recordImage(ctx, store, connect, m.App, revision, name, ref, images, out)
					},
				})
			default:
				add(kernel.Step{
					Name: "pull-" + name, Change: fmt.Sprintf("use image %s for %s", w.Image, name),
					Apply: func(ctx context.Context, out io.Writer) error {
						e, err := connect(ctx)
						if err != nil {
							return err
						}
						if !e.HasImage(ctx, w.Image) {
							if err := e.Pull(ctx, w.Image, out); err != nil {
								return err
							}
						}
						images[name] = w.Image
						return store.SaveRevision(ctx, state.Revision{App: m.App, ID: revision, Status: state.RevisionFailed, Manifest: manifestJSON, Images: images, Containers: containers, Source: from.commit, CreatedAt: time.Now().UTC()})
					},
				})
			}
		}
	}
	add(kernel.Step{
		Name: "start", Change: fmt.Sprintf("start %d container(s) for revision %s", len(containers), revision),
		Note: secretsNote(m),
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			rev, err := store.GetRevision(ctx, m.App, revision)
			if err != nil {
				return err
			}
			// A deploy runs with the current secrets and pins that version;
			// a rollback runs with the version the revision had.
			values, version, err := d.secretsFor(rev, from == nil)
			if err != nil {
				return err
			}
			if err := checkSecrets(m, values); err != nil {
				return err
			}
			if rev.SecretsVersion != version {
				rev.SecretsVersion = version
				if err := store.SaveRevision(ctx, *rev); err != nil {
					return err
				}
			}
			for _, name := range m.WorkloadNames() {
				w := m.Workloads[name]
				image := rev.Images[name]
				if image == "" {
					return fmt.Errorf("no image recorded for %s", name)
				}
				spec, err := containerSpec(m, name, w, revision, image, values)
				if err != nil {
					return err
				}
				u, err := isolate(ctx, e, &spec, w, image)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				// Cron workloads start later, but their volumes are
				// readied now, once, instead of on every run.
				if err := ownVolumes(ctx, e, m, w, u, out); err != nil {
					return err
				}
				if !w.LongRunning() {
					continue
				}
				if err := e.Run(ctx, spec); err != nil {
					return err
				}
				fmt.Fprintf(out, "%s running as %s%s\n", spec.Name, u, isolationWord(w))
			}
			if version > 0 {
				fmt.Fprintf(out, "secrets version %d\n", version)
			}
			return nil
		},
	})
	add(kernel.Step{
		Name: "ready", Change: "wait until every workload answers",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			for _, name := range m.WorkloadNames() {
				w := m.Workloads[name]
				if !w.Serves() {
					continue
				}
				if err := waitReady(ctx, e, containers[name], w); err != nil {
					// The new containers never went live; take them down.
					for _, c := range containers {
						_ = e.Remove(ctx, c, 5*time.Second)
					}
					_ = store.SetRevisionStatus(ctx, m.App, revision, state.RevisionFailed)
					return fmt.Errorf("%s: %w", name, err)
				}
				fmt.Fprintf(out, "%s ready\n", name)
			}
			return nil
		},
	})
	if len(m.Checks) > 0 {
		add(kernel.Step{
			Name: "checks", Change: fmt.Sprintf("run %d check(s) against the new containers", len(m.Checks)),
			// Before any traffic reaches it: a revision that answers wrong
			// never goes live.
			Apply: func(ctx context.Context, out io.Writer) error {
				e, err := connect(ctx)
				if err != nil {
					return err
				}
				for _, c := range m.Checks {
					if err := runCheckDirect(ctx, e, m, containers, c); err != nil {
						for _, name := range containers {
							_ = e.Remove(ctx, name, 5*time.Second)
						}
						_ = store.SetRevisionStatus(ctx, m.App, revision, state.RevisionFailed)
						return err
					}
					fmt.Fprintf(out, "%s ok\n", c.URL)
				}
				return nil
			},
		})
	}
	add(kernel.Step{
		Name: "switch", Change: "route the edge to the new revision",
		Apply: func(ctx context.Context, out io.Writer) error {
			if err := store.Activate(ctx, m.App, revision, time.Now().UTC()); err != nil {
				return err
			}
			cfg, err := edgeConfig(ctx, store)
			if err != nil {
				return err
			}
			if err := edge.NewAdmin().Load(ctx, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "edge routes to %s\n", revision)
			return nil
		},
	})
	if len(hosts) > 0 {
		add(kernel.Step{
			Name: "certificates", Change: "wait for certificates for " + strings.Join(hosts, ", "),
			Apply: func(ctx context.Context, out io.Writer) error {
				for _, host := range hosts {
					why, err := edge.WaitCertificate(ctx, host, "127.0.0.1:443", 3*time.Minute)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "%s: %s\n", host, why)
				}
				return nil
			},
		})
	}
	if len(m.Checks) > 0 {
		add(kernel.Step{
			Name: "verify", Change: fmt.Sprintf("run %d check(s) through the edge", len(m.Checks)),
			// The checks already passed against the containers; this proves
			// the edge and the certificates. If it fails, the previous
			// revision comes back before anyone notices.
			Apply: func(ctx context.Context, out io.Writer) error {
				for _, c := range m.Checks {
					if err := runCheck(ctx, c); err != nil {
						revert, revertErr := revertSwitch(ctx, store, connect, m.App, revision, containers)
						if revertErr != nil {
							return fmt.Errorf("%w; and putting the previous revision back failed too: %v", err, revertErr)
						}
						return fmt.Errorf("%w; %s", err, revert)
					}
					fmt.Fprintf(out, "%s ok\n", c.URL)
				}
				return nil
			},
		})
	}
	add(kernel.Step{
		Name: "retire", Change: "stop the revision this one replaces",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			revs, err := store.Revisions(ctx, m.App)
			if err != nil {
				return err
			}
			for _, r := range revs {
				if r.ID == revision || r.Status == state.RevisionActive {
					continue
				}
				for _, c := range r.Containers {
					if err := e.Remove(ctx, c, 10*time.Second); err != nil {
						return err
					}
				}
				if r.Status == state.RevisionPrevious {
					fmt.Fprintf(out, "%s stopped, kept for rollback\n", r.ID)
				}
			}
			return pruneRecords(ctx, store, d.Secrets, m.App, d.Addresses(ctx), out)
		},
	})
	return plan, nil
}

func noteCommit(commit string) string {
	if commit == "" {
		return ""
	}
	return "commit " + commit
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}

func imageLabels(app, workload, revision string) map[string]string {
	return map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: app, docker.LabelWorkload: workload, docker.LabelRevision: revision}
}

// recordImage stores the digest reference of a built image on the revision.
func recordImage(ctx context.Context, store *state.Store, connect func(context.Context) (*docker.Engine, error), app, revision, workload, ref string, images map[string]string, out io.Writer) error {
	e, err := connect(ctx)
	if err != nil {
		return err
	}
	digest, err := e.Digest(ctx, ref)
	if err != nil {
		return err
	}
	rev, err := store.GetRevision(ctx, app, revision)
	if err != nil {
		return err
	}
	rev.Images[workload] = digest
	images[workload] = digest
	fmt.Fprintf(out, "%s\n", digest)
	return store.SaveRevision(ctx, *rev)
}

// filtered keeps a build's useful lines and drops the progress noise.
func filtered(out io.Writer) io.Writer { return &lineFilter{out: out} }

type lineFilter struct {
	out     io.Writer
	partial strings.Builder
}

func (f *lineFilter) Write(p []byte) (int, error) {
	for _, c := range p {
		if c != '\n' {
			f.partial.WriteByte(c)
			continue
		}
		if line, ok := docker.TrimBuildLine(f.partial.String()); ok {
			fmt.Fprintln(f.out, line)
		}
		f.partial.Reset()
	}
	return len(p), nil
}

func containerSpec(m *manifest.Manifest, name string, w manifest.Workload, revision, image string, values map[string]string) (docker.Spec, error) {
	spec := docker.Spec{
		Name:     docker.ContainerName(m.App, name, revision),
		Image:    image,
		Cmd:      w.Command,
		Labels:   imageLabels(m.App, name, revision),
		Networks: []string{docker.AppNetwork(m.App)},
		Restart:  true,
	}
	if w.Serves() {
		// The one network it shares with the edge, and with no other app.
		spec.Networks = append(spec.Networks, edge.AppNetwork(m.App))
	}
	spec.Env = []string{"QUARK_APP=" + m.App, "QUARK_WORKLOAD=" + name, "QUARK_REVISION=" + revision}
	for _, k := range sortedKeys(w.Env) {
		spec.Env = append(spec.Env, k+"="+w.Env[k])
	}
	for _, k := range w.Secrets {
		spec.Env = append(spec.Env, k+"="+values[k])
	}
	if m.PostgresVersion() != "" {
		spec.Env = append(spec.Env, DatabaseURLName+"="+values[DatabaseURLName])
	}
	for _, mt := range w.Mounts {
		spec.Mounts = append(spec.Mounts, docker.VolumeName(m.App, mt.Volume)+":"+mt.Path)
	}
	if w.Resources.Memory != "" {
		bytes, err := parseSize(w.Resources.Memory)
		if err != nil {
			return spec, err
		}
		spec.MemoryBytes = bytes
	}
	if w.Resources.CPUs > 0 {
		spec.NanoCPUs = int64(w.Resources.CPUs * 1e9)
	}
	return spec, nil
}

func parseSize(s string) (int64, error) {
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q", s)
	}
	switch s[len(s)-1] {
	case 'k':
		return n << 10, nil
	case 'm':
		return n << 20, nil
	case 'g':
		return n << 30, nil
	}
	return 0, fmt.Errorf("size %q", s)
}

// waitReady waits for a workload's container to accept connections, and
// for its health path to answer when it has one.
func waitReady(ctx context.Context, e *docker.Engine, container string, w manifest.Workload) error {
	timeout := 60 * time.Second
	if w.Health != nil && w.Health.Timeout != "" {
		if d, err := time.ParseDuration(w.Health.Timeout); err == nil {
			timeout = d
		}
	}
	port := w.Port
	if w.Kind == manifest.Static {
		port, _ = strconv.Atoi(docker.StaticPort)
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		info, err := e.Inspect(ctx, container)
		switch {
		case err != nil:
			last = err.Error()
		case !info.Running || info.Restarts > 0:
			// Its last words say why, isolation included (a binary that
			// wants a capability, a write to a read-only path).
			words := lastWords(e.LogTail(ctx, container, 5))
			return fmt.Errorf("the container exited (exit code %d, restarted %d times): %s", info.ExitCode, info.Restarts, words)
		default:
			ip := info.IPs[docker.EdgeNetwork]
			if ip == "" {
				for _, v := range info.IPs {
					ip = v
				}
			}
			if ip != "" {
				if w.Health != nil && w.Health.Path != "" {
					resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://%s:%d%s", ip, port, w.Health.Path))
					if err == nil {
						resp.Body.Close()
						if resp.StatusCode < 400 {
							return nil
						}
						last = fmt.Sprintf("%s answered %s", w.Health.Path, resp.Status)
					} else {
						last = err.Error()
					}
				} else {
					conn, err := (&net_Dialer{}).dial(ctx, fmt.Sprintf("%s:%d", ip, port))
					if err == nil {
						conn.Close()
						return nil
					}
					last = err.Error()
				}
			} else {
				last = "no network address yet"
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s: %s", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ReloadEdge gives the edge the configuration the active revisions call
// for, when it is up. The daemon does this at start, so a quark that
// changed how it configures the edge takes effect without a deploy.
func ReloadEdge(ctx context.Context, store *state.Store) error {
	admin := edge.NewAdmin()
	if !admin.Answers(ctx) {
		return nil
	}
	cfg, err := EdgeConfig(ctx, store)
	if err != nil {
		return err
	}
	return admin.Load(ctx, cfg)
}

// UpgradeEdge replaces an edge an older quark made, keeping its routes,
// and then gives it the current configuration. A machine without an edge
// yet is left for host setup.
func UpgradeEdge(ctx context.Context, store *state.Store, out io.Writer) error {
	e, err := docker.Connect(ctx)
	if err != nil {
		return err
	}
	defer e.Close()
	info, err := e.Inspect(ctx, edge.Container)
	if err != nil {
		return nil
	}
	if info.Labels[edge.LayoutLabel] != edge.Layout {
		boot, err := EdgeConfig(ctx, store)
		if err != nil {
			return err
		}
		if err := edge.Ensure(ctx, e, boot, out); err != nil {
			return err
		}
	}
	return ReloadEdge(ctx, store)
}

// servesAny reports whether any of an app's workloads takes traffic from
// the edge.
func servesAny(m *manifest.Manifest) bool {
	for _, name := range m.WorkloadNames() {
		if m.Workloads[name].Serves() {
			return true
		}
	}
	return false
}

// isolationWord says in a few words when a workload asked to be less
// isolated than the default.
func isolationWord(w manifest.Workload) string {
	switch {
	case w.Privileged:
		return ", privileged as its manifest says"
	case w.WritableRoot:
		return ", root filesystem writable as its manifest says"
	}
	return ""
}

// EdgeConfig builds the edge's whole configuration from every active revision.
func EdgeConfig(ctx context.Context, store *state.Store) ([]byte, error) {
	return edgeConfig(ctx, store)
}

// HookRoute is the path webhooks arrive on, as the edge routes it.
const HookRoute = "/_quark/hook/"

// lastWords picks the last line a container wrote, for an error message.
func lastWords(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "it wrote nothing; see quark logs"
}

// edgeConfig builds the edge's whole configuration from every active revision.
func edgeConfig(ctx context.Context, store *state.Store) ([]byte, error) {
	active, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	hooks, err := store.GitHooks(ctx)
	if err != nil {
		return nil, err
	}
	hooked := map[string]bool{}
	for _, h := range hooks {
		hooked[h.App] = true
	}
	var routes []edge.Route
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			return nil, fmt.Errorf("revision %s/%s manifest: %w", rev.App, rev.ID, err)
		}
		if hosts := m.Hosts(); hooked[rev.App] && len(hosts) > 0 {
			// Webhooks for the app arrive on its first host and go to
			// quark, not to the app.
			routes = append(routes, edge.Route{Host: hosts[0], Path: HookRoute, Dial: edge.HooksDial})
		}
		for _, name := range m.WorkloadNames() {
			w := m.Workloads[name]
			port := w.Port
			if w.Kind == manifest.Static {
				port, _ = strconv.Atoi(docker.StaticPort)
			}
			for _, r := range w.Routes {
				routes = append(routes, edge.Route{Host: r.Host, Path: r.NormalizedPath(), Dial: fmt.Sprintf("%s:%d", rev.Containers[name], port)})
			}
		}
	}
	return edge.Config(routes)
}

func checkTimeout(c manifest.Check) time.Duration {
	if c.Within != "" {
		if d, err := time.ParseDuration(c.Within); err == nil {
			return d
		}
	}
	return 10 * time.Second
}

// RunCheck performs one manifest check against the live URL, through the
// edge, the way the watcher does between deploys.
func RunCheck(ctx context.Context, c manifest.Check) error { return runCheck(ctx, c) }

// runCheck performs one manifest check against the live URL, through the
// edge, resolving the name the way the world does.
func runCheck(ctx context.Context, c manifest.Check) error {
	within := checkTimeout(c)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	return evaluateCheck(c, &http.Client{Timeout: within, Transport: publicTransport()}, req, within)
}

// runCheckDirect performs a check against the new container that would
// serve the URL, before the edge routes anything to it.
func runCheckDirect(ctx context.Context, e *docker.Engine, m *manifest.Manifest, containers map[string]string, c manifest.Check) error {
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("check %s: %w", c.URL, err)
	}
	name, w, ok := m.WorkloadFor(u.Hostname(), u.Path)
	if !ok {
		return fmt.Errorf("check %s: no workload routes %s%s", c.URL, u.Hostname(), u.Path)
	}
	info, err := e.Inspect(ctx, containers[name])
	if err != nil {
		return fmt.Errorf("check %s: %w", c.URL, err)
	}
	ip := containerIP(info)
	if ip == "" {
		return fmt.Errorf("check %s: %s has no address", c.URL, containers[name])
	}
	port := w.Port
	if w.Kind == manifest.Static {
		port, _ = strconv.Atoi(docker.StaticPort)
	}
	within := checkTimeout(c)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d%s", ip, port, u.RequestURI()), nil)
	if err != nil {
		return err
	}
	req.Host = u.Host
	return evaluateCheck(c, &http.Client{Timeout: within}, req, within)
}

func evaluateCheck(c manifest.Check, client *http.Client, req *http.Request, within time.Duration) error {
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("check %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	took := time.Since(started)
	want := c.Status
	if want == 0 {
		want = http.StatusOK
	}
	if resp.StatusCode != want {
		return fmt.Errorf("check %s: answered %s, want %d", c.URL, resp.Status, want)
	}
	if c.Contains != "" && !strings.Contains(string(body), c.Contains) {
		return fmt.Errorf("check %s: the page doesn't contain %q", c.URL, c.Contains)
	}
	if took > within {
		return fmt.Errorf("check %s: took %s, longer than %s", c.URL, took.Round(time.Millisecond), within)
	}
	return nil
}

func containerIP(info *docker.Info) string {
	if ip := info.IPs[docker.EdgeNetwork]; ip != "" {
		return ip
	}
	for _, ip := range info.IPs {
		return ip
	}
	return ""
}

// revertSwitch puts the previous revision back on the edge after a live
// verification failed, or routes nothing to the app when there is none,
// and takes the failed revision's containers down.
func revertSwitch(ctx context.Context, store *state.Store, connect func(context.Context) (*docker.Engine, error), app, failed string, containers map[string]string) (string, error) {
	prev, err := store.RevisionWithStatus(ctx, app, state.RevisionPrevious)
	if err != nil {
		return "", err
	}
	if prev != nil {
		if err := store.Activate(ctx, app, prev.ID, time.Now().UTC()); err != nil {
			return "", err
		}
	}
	if err := store.SetRevisionStatus(ctx, app, failed, state.RevisionFailed); err != nil {
		return "", err
	}
	cfg, err := edgeConfig(ctx, store)
	if err != nil {
		return "", err
	}
	if err := edge.NewAdmin().Load(ctx, cfg); err != nil {
		return "", err
	}
	if e, err := connect(ctx); err == nil {
		for _, c := range containers {
			_ = e.Remove(ctx, c, 5*time.Second)
		}
	}
	if prev == nil {
		return "the edge routes nothing to " + app + " now", nil
	}
	return "the edge is back on " + prev.ID, nil
}

// sourceCommit reads the source's git commit, when it is a checkout.
func sourceCommit(ctx context.Context, source string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", source, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Rollback is the Definition for RollbackKind.
type Rollback struct {
	Deploy Deploy
}

// RollbackInput says which revision to make active again.
type RollbackInput struct {
	App string `json:"app"`
	// Revision is the one to return to; empty means the previous one.
	Revision string `json:"revision,omitempty"`
}

// Kind implements kernel.Definition.
func (Rollback) Kind() string { return RollbackKind }

// Plan implements kernel.Definition.
func (r Rollback) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in RollbackInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("rollback input: %w", err)
	}
	store := r.Deploy.Store
	var rev *state.Revision
	var err error
	if in.Revision == "" {
		rev, err = store.RevisionWithStatus(ctx, in.App, state.RevisionPrevious)
		if err != nil {
			return nil, err
		}
		if rev == nil {
			return nil, fmt.Errorf("%s has no previous revision to roll back to", in.App)
		}
	} else {
		rev, err = store.GetRevision(ctx, in.App, in.Revision)
		if err != nil {
			return nil, fmt.Errorf("revision %s of %s: %w", in.Revision, in.App, err)
		}
	}
	if rev.Status == state.RevisionActive {
		return nil, fmt.Errorf("%s is already the active revision of %s", rev.ID, in.App)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return nil, err
	}
	return r.Deploy.rollout(ctx, &m, rev.ID, nil)
}

// publicTransport resolves names the way the world does, through a public
// resolver, so a check isn't fooled by the machine's own cache.
func publicTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Resolver: edge.PublicResolver}
	return &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second}
}

// secretsFor returns the values a revision runs with: the current version
// for a deploy, the pinned one for a rollback.
func (d Deploy) secretsFor(rev *state.Revision, pinned bool) (map[string]string, int, error) {
	if pinned {
		values, err := d.Secrets.Load(rev.App, rev.SecretsVersion)
		return values, rev.SecretsVersion, err
	}
	return d.Secrets.LoadCurrent(rev.App)
}

// checkSecrets fails before anything starts when a workload's secrets
// aren't all set.
func checkSecrets(m *manifest.Manifest, values map[string]string) error {
	var missing []string
	for _, name := range m.WorkloadNames() {
		for _, n := range secrets.Missing(values, m.Workloads[name].Secrets) {
			missing = append(missing, fmt.Sprintf("%s (for %s)", n, name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("secrets not set: %s; set each with quark secret set %s NAME", strings.Join(missing, ", "), m.App)
	}
	return nil
}

func secretsNote(m *manifest.Manifest) string {
	var names []string
	for _, name := range m.WorkloadNames() {
		names = append(names, m.Workloads[name].Secrets...)
	}
	if m.PostgresVersion() != "" {
		names = append(names, DatabaseURLName)
	}
	if len(names) == 0 {
		return ""
	}
	return "secrets: " + strings.Join(names, ", ")
}

func dataSummary(m *manifest.Manifest) string {
	var parts []string
	if v := m.PostgresVersion(); v != "" {
		parts = append(parts, "postgres "+v)
	}
	for _, v := range sortedKeys(m.Data.Volumes) {
		parts = append(parts, "volume "+v)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ", ")
}

func restoreNote(r Restore) string {
	var parts []string
	if r.Postgres != "" {
		parts = append(parts, "database from "+r.Postgres)
	}
	for _, v := range sortedKeys(r.Volumes) {
		parts = append(parts, v+" from "+r.Volumes[v])
	}
	if len(parts) == 0 {
		return ""
	}
	return "restore " + strings.Join(parts, "; ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
