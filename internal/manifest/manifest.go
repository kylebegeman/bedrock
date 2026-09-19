// Package manifest is the one file an app needs: what it runs, where it is
// reached, how it is checked, and what it may use. quark.yaml lives in the
// app's repository.
package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the manifest's name in an app's repository.
const FileName = "quark.yaml"

// Manifest describes one app.
type Manifest struct {
	// App is the app's name: lowercase letters, digits and hyphens.
	App string `yaml:"app" json:"app"`
	// Description is one line about what the app is for.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Owner is who the app belongs to, such as personal or braintreelabs.
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
	// Repo is where the source lives.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Workloads are the containers the app runs, by name.
	Workloads map[string]Workload `yaml:"workloads" json:"workloads"`
	// Checks run after every deploy and again as health.
	Checks []Check `yaml:"checks,omitempty" json:"checks,omitempty"`
}

// Kind is what a workload is.
type Kind string

const (
	// Web serves HTTP on a port and gets routes.
	Web Kind = "web"
	// Static serves files from a directory in the source, and gets routes.
	Static Kind = "static"
	// Worker runs without listening.
	Worker Kind = "worker"
)

// Workload is one container the app runs.
type Workload struct {
	Kind Kind `yaml:"kind" json:"kind"`
	// Image is a pulled image reference, when the workload isn't built here.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// Build says how to build the image from the source.
	Build *Build `yaml:"build,omitempty" json:"build,omitempty"`
	// Dir is the directory to serve, for a static workload.
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
	// Port is the container port a web workload listens on.
	Port int `yaml:"port,omitempty" json:"port,omitempty"`
	// Routes are the hostnames and paths that reach this workload.
	Routes []Route `yaml:"routes,omitempty" json:"routes,omitempty"`
	// Env is plain configuration. Secrets are named in Secrets instead.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Secrets are the names of secrets the workload needs.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Command replaces the image's command.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	// Health is how quark knows the workload is ready.
	Health *Health `yaml:"health,omitempty" json:"health,omitempty"`
	// Resources bound what the workload may use.
	Resources Resources `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// Build says how to build a workload's image.
type Build struct {
	// Context is the build directory, relative to the source. Default ".".
	Context string `yaml:"context,omitempty" json:"context,omitempty"`
	// Dockerfile is relative to the context. Default "Dockerfile".
	Dockerfile string `yaml:"dockerfile,omitempty" json:"dockerfile,omitempty"`
	// Target is a stage in a multi-stage Dockerfile.
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	// Args are build arguments.
	Args map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
}

// Route is one way in from the edge.
type Route struct {
	Host string `yaml:"host" json:"host"`
	// Path is a prefix; "/" (the default) takes everything not claimed by
	// a longer prefix on the same host.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

// Health is how a workload says it is ready.
type Health struct {
	// Path is an HTTP path that must answer 2xx or 3xx. Empty means the
	// port only has to accept a connection.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Timeout is how long to wait for readiness, as a Go duration. Default 60s.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Resources bound a workload.
type Resources struct {
	// Memory is a limit such as 512m or 2g.
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	// CPUs is a limit such as 1 or 0.5.
	CPUs float64 `yaml:"cpus,omitempty" json:"cpus,omitempty"`
}

// Check is a URL that must answer a certain way after a deploy.
type Check struct {
	URL string `yaml:"url" json:"url"`
	// Status is the expected code. Default 200.
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Contains is text the body must include.
	Contains string `yaml:"contains,omitempty" json:"contains,omitempty"`
	// Within is the longest acceptable response time, as a Go duration.
	Within string `yaml:"within,omitempty" json:"within,omitempty"`
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	hostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
	envPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	sizePattern = regexp.MustCompile(`^[0-9]+[kmg]$`)
)

// Parse reads a manifest from YAML and validates it.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load reads dir/quark.yaml.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Validate checks a manifest. The errors name the field in the person's
// own terms.
func (m *Manifest) Validate() error {
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if !namePattern.MatchString(m.App) {
		fail("app: %q must be lowercase letters, digits and hyphens, up to 40 characters", m.App)
	}
	if len(m.Workloads) == 0 {
		fail("workloads: an app needs at least one")
	}
	claimed := map[string]string{} // host+path -> workload
	for _, name := range m.WorkloadNames() {
		w := m.Workloads[name]
		at := "workloads." + name
		if !namePattern.MatchString(name) {
			fail("%s: the name must be lowercase letters, digits and hyphens", at)
		}
		switch w.Kind {
		case Web, Worker:
			if (w.Image == "") == (w.Build == nil) {
				fail("%s: give either image or build", at)
			}
			if w.Dir != "" {
				fail("%s: dir is for static workloads", at)
			}
		case Static:
			if w.Dir == "" {
				fail("%s: a static workload needs dir, the directory to serve", at)
			}
			if w.Image != "" || w.Build != nil || w.Port != 0 {
				fail("%s: a static workload has dir only, no image, build or port", at)
			}
		case "":
			fail("%s: kind is required: web, static or worker", at)
		default:
			fail("%s: kind %q isn't one of web, static, worker", at, w.Kind)
		}
		if w.Kind == Web {
			if w.Port < 1 || w.Port > 65535 {
				fail("%s: a web workload needs port, the container port it listens on", at)
			}
			if len(w.Routes) == 0 {
				fail("%s: a web workload needs at least one route", at)
			}
		}
		if w.Kind == Static && len(w.Routes) == 0 {
			fail("%s: a static workload needs at least one route", at)
		}
		if w.Kind == Worker && len(w.Routes) > 0 {
			fail("%s: a worker has no routes", at)
		}
		for i, r := range w.Routes {
			ra := fmt.Sprintf("%s.routes[%d]", at, i)
			if !hostPattern.MatchString(r.Host) {
				fail("%s: host %q isn't a hostname", ra, r.Host)
			}
			path := r.Path
			if path == "" {
				path = "/"
			}
			if !strings.HasPrefix(path, "/") || strings.Contains(path, "*") {
				fail("%s: path %q must start with / and is a prefix, without wildcards", ra, r.Path)
			}
			key := r.Host + " " + path
			if other, taken := claimed[key]; taken {
				fail("%s: %s %s is already routed to %s", ra, r.Host, path, other)
			}
			claimed[key] = name
		}
		for k := range w.Env {
			if !envPattern.MatchString(k) {
				fail("%s.env: %q must be an UPPER_CASE name", at, k)
			}
		}
		for _, s := range w.Secrets {
			if !envPattern.MatchString(s) {
				fail("%s.secrets: %q must be an UPPER_CASE name", at, s)
			}
			if _, both := w.Env[s]; both {
				fail("%s: %s is both a secret and a plain env value", at, s)
			}
		}
		if w.Resources.Memory != "" && !sizePattern.MatchString(w.Resources.Memory) {
			fail("%s.resources.memory: %q must look like 512m or 2g", at, w.Resources.Memory)
		}
		if w.Resources.CPUs < 0 || w.Resources.CPUs > 64 {
			fail("%s.resources.cpus: must be 0 to 64", at)
		}
		if w.Health != nil && w.Health.Path != "" && !strings.HasPrefix(w.Health.Path, "/") {
			fail("%s.health.path: must start with /", at)
		}
		if w.Build != nil && (strings.HasPrefix(w.Build.Context, "/") || strings.Contains(w.Build.Context, "..")) {
			fail("%s.build.context: must be inside the source", at)
		}
		if w.Dir != "" && (strings.HasPrefix(w.Dir, "/") || strings.Contains(w.Dir, "..")) {
			fail("%s.dir: must be inside the source", at)
		}
	}
	for i, c := range m.Checks {
		if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
			fail("checks[%d]: url must start with https:// or http://", i)
		}
		if c.Status < 0 || c.Status > 599 {
			fail("checks[%d]: status must be an HTTP status", i)
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

// WorkloadNames returns the workload names in a stable order.
func (m *Manifest) WorkloadNames() []string {
	names := make([]string, 0, len(m.Workloads))
	for name := range m.Workloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Hosts returns every hostname the app is reached at, in order.
func (m *Manifest) Hosts() []string {
	seen := map[string]bool{}
	var hosts []string
	for _, name := range m.WorkloadNames() {
		for _, r := range m.Workloads[name].Routes {
			if !seen[r.Host] {
				seen[r.Host] = true
				hosts = append(hosts, r.Host)
			}
		}
	}
	sort.Strings(hosts)
	return hosts
}

// Path returns a route's path with its default applied.
func (r Route) NormalizedPath() string {
	if r.Path == "" {
		return "/"
	}
	return strings.TrimSuffix(r.Path, "/") + "/"
}
