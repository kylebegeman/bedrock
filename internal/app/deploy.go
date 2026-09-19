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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/state"
)

// DeployKind deploys a source directory as a new revision of its app.
const DeployKind = "app.deploy"

// RollbackKind makes an earlier revision active again.
const RollbackKind = "app.rollback"

// Deploy is the Definition for DeployKind.
type Deploy struct {
	Store *state.Store
	// Addresses are this machine's public addresses, for the DNS check.
	Addresses func(ctx context.Context) []string
}

// DeployInput says what to deploy.
type DeployInput struct {
	// Source is the directory holding quark.yaml and the code.
	Source string `json:"source"`
	// Revision names the new revision; the CLI makes one from the clock.
	Revision string `json:"revision"`
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
	m, err := manifest.Load(in.Source)
	if err != nil {
		return nil, err
	}
	commit := sourceCommit(ctx, in.Source)
	return d.rollout(ctx, m, in.Revision, &buildFrom{source: in.Source, commit: commit})
}

// buildFrom says images come from a source directory; nil means they are
// already recorded (a rollback).
type buildFrom struct {
	source string
	commit string
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
		containers[name] = docker.ContainerName(m.App, name, revision)
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
		Name: "edge", Change: "run the edge and the app's network",
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			if err := edge.Ensure(ctx, e, out); err != nil {
				return err
			}
			return e.EnsureNetwork(ctx, docker.AppNetwork(m.App))
		},
	})
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
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := connect(ctx)
			if err != nil {
				return err
			}
			rev, err := store.GetRevision(ctx, m.App, revision)
			if err != nil {
				return err
			}
			for _, name := range m.WorkloadNames() {
				w := m.Workloads[name]
				image := rev.Images[name]
				if image == "" {
					return fmt.Errorf("no image recorded for %s", name)
				}
				spec, err := containerSpec(m, name, w, revision, image)
				if err != nil {
					return err
				}
				if err := e.Run(ctx, spec); err != nil {
					return err
				}
				fmt.Fprintf(out, "%s running\n", spec.Name)
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
				if w.Kind == manifest.Worker {
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
	if len(hosts) > 0 {
		add(kernel.Step{
			Name: "dns", Change: "confirm " + strings.Join(hosts, ", ") + " point at this machine",
			Apply: func(ctx context.Context, out io.Writer) error {
				addrs := d.Addresses(ctx)
				for _, host := range hosts {
					ok, pointsAt, err := edge.Resolves(ctx, host, addrs)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New(edge.DNSProblem(host, pointsAt, addrs))
					}
					fmt.Fprintf(out, "%s points here\n", host)
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
			Name: "checks", Change: fmt.Sprintf("run %d check(s)", len(m.Checks)),
			Apply: func(ctx context.Context, out io.Writer) error {
				for _, c := range m.Checks {
					if err := runCheck(ctx, c); err != nil {
						return err
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
			return nil
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

func containerSpec(m *manifest.Manifest, name string, w manifest.Workload, revision, image string) (docker.Spec, error) {
	spec := docker.Spec{
		Name:     docker.ContainerName(m.App, name, revision),
		Image:    image,
		Cmd:      w.Command,
		Labels:   imageLabels(m.App, name, revision),
		Networks: []string{docker.AppNetwork(m.App)},
		Restart:  true,
	}
	if w.Kind != manifest.Worker {
		spec.Networks = append(spec.Networks, docker.EdgeNetwork)
	}
	for k, v := range w.Env {
		spec.Env = append(spec.Env, k+"="+v)
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
		case !info.Running:
			return fmt.Errorf("the container stopped (%s); see quark logs", info.Status)
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

// edgeConfig builds the edge's whole configuration from every active revision.
func edgeConfig(ctx context.Context, store *state.Store) ([]byte, error) {
	active, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	var routes []edge.Route
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			return nil, fmt.Errorf("revision %s/%s manifest: %w", rev.App, rev.ID, err)
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

// runCheck performs one manifest check against the live URL.
func runCheck(ctx context.Context, c manifest.Check) error {
	within := 10 * time.Second
	if c.Within != "" {
		if d, err := time.ParseDuration(c.Within); err == nil {
			within = d
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	started := time.Now()
	resp, err := (&http.Client{Timeout: within}).Do(req)
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
