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
	"time"

	"github.com/robfig/cron/v3"
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
	// Data is what the app keeps: a database and named volumes.
	Data *Data `yaml:"data,omitempty" json:"data,omitempty"`
	// Backup says when the data is backed up and drilled. An app with data
	// is backed up nightly and drilled weekly unless this says otherwise.
	Backup *Backup `yaml:"backup,omitempty" json:"backup,omitempty"`
}

// Backup is an app's backup policy.
type Backup struct {
	// Schedule is a five-field cron schedule, in UTC. Default "0 3 * * *".
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	// Drill is when the latest backup is restored beside the app and
	// verified. Default "0 4 * * 0", Sunday.
	Drill string `yaml:"drill,omitempty" json:"drill,omitempty"`
	// Keep is how many daily, weekly and monthly backups stay. Default 7,
	// 4 and 6.
	Keep *Keep `yaml:"keep,omitempty" json:"keep,omitempty"`
	// Verify is a query the drill runs on the restored database.
	Verify *Verify `yaml:"verify,omitempty" json:"verify,omitempty"`
	// Off turns backups off for an app whose data isn't worth keeping.
	Off bool `yaml:"off,omitempty" json:"off,omitempty"`
}

// Keep is a retention policy.
type Keep struct {
	Daily   int `yaml:"daily,omitempty" json:"daily,omitempty"`
	Weekly  int `yaml:"weekly,omitempty" json:"weekly,omitempty"`
	Monthly int `yaml:"monthly,omitempty" json:"monthly,omitempty"`
}

// Verify is what a drill checks in the restored database.
type Verify struct {
	// SQL must return one number.
	SQL string `yaml:"sql" json:"sql"`
	// AtLeast is the smallest acceptable result. Default 1.
	AtLeast int `yaml:"at_least,omitempty" json:"at_least,omitempty"`
}

// Defaults for backups.
const (
	DefaultBackupSchedule = "0 3 * * *"
	DefaultDrillSchedule  = "0 4 * * 0"
)

// DefaultKeep is the retention an app gets unless it says otherwise.
var DefaultKeep = Keep{Daily: 7, Weekly: 4, Monthly: 6}

// ReservedApp is the name the machine's own things use, such as the
// integration credentials. No app may take it.
const ReservedApp = "quark"

// reservedApps are names whose containers, networks or volumes would
// collide with quark's own: quark-edge, quark-registry, quark-restic-cache.
var reservedApps = map[string]bool{ReservedApp: true, "edge": true, "registry": true, "restic": true}

// DatabaseVolume is the name of the volume that holds an app's database;
// no declared volume may take it.
const DatabaseVolume = "postgres"

// Data is an app's state, kept across revisions.
type Data struct {
	// Postgres gives the app its own database. Its URL reaches every
	// workload as DATABASE_URL.
	Postgres *Postgres `yaml:"postgres,omitempty" json:"postgres,omitempty"`
	// Volumes are named directories workloads mount.
	Volumes map[string]Volume `yaml:"volumes,omitempty" json:"volumes,omitempty"`
}

// Postgres configures the app's database.
type Postgres struct {
	// Version is the major version. Default 16.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

// Volume is a named directory an app keeps.
type Volume struct {
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Mount puts a volume in a workload.
type Mount struct {
	Volume string `yaml:"volume" json:"volume"`
	Path   string `yaml:"path" json:"path"`
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
	// Cron runs its command on a schedule and exits.
	Cron Kind = "cron"
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
	// Mounts put the app's volumes in this workload.
	Mounts []Mount `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	// Schedule is a cron workload's five-field schedule, in UTC.
	Schedule string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	// Timeout is how long a cron run may take. Default 1h.
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// User runs the container as this user (uid, name or uid:gid) instead
	// of the image's own.
	User string `yaml:"user,omitempty" json:"user,omitempty"`
	// Privileged keeps every capability and lets the container gain
	// privileges: for a workload that runs other containers, such as
	// Loom's Runner. Everything else runs with no capabilities and can't
	// gain any.
	Privileged bool `yaml:"privileged,omitempty" json:"privileged,omitempty"`
	// Capabilities are the kernel capabilities to keep, such as
	// NET_BIND_SERVICE, when an image needs a few. Every other one is
	// dropped.
	Capabilities []string `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	// WritableRoot lets the workload write to its root filesystem. By
	// default it is read-only, with /tmp in memory.
	WritableRoot bool `yaml:"writable_root,omitempty" json:"writable_root,omitempty"`
	// Tmpfs are more paths the workload may write to, kept in memory, such
	// as a framework's cache directory.
	Tmpfs []string `yaml:"tmpfs,omitempty" json:"tmpfs,omitempty"`
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
	// DNS says whether quark keeps the host's record in Cloudflare:
	// "direct" makes a record that names this machine, "proxied" one
	// behind Cloudflare's proxy. Empty leaves the record alone and only
	// checks that it points here.
	DNS DNSMode `yaml:"dns,omitempty" json:"dns,omitempty"`
}

// DNSMode is how a route's record is kept.
type DNSMode string

const (
	// DNSManual leaves the record to the person; the deploy checks it.
	DNSManual DNSMode = ""
	// DNSDirect keeps a record that points straight at the machine.
	DNSDirect DNSMode = "direct"
	// DNSProxied keeps a record behind Cloudflare's proxy.
	DNSProxied DNSMode = "proxied"
)

// Managed reports whether quark keeps the record.
func (m DNSMode) Managed() bool { return m == DNSDirect || m == DNSProxied }

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
	capPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	userPattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*(:[a-z0-9_][a-z0-9_-]*)?$`)
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
	} else if reservedApps[m.App] {
		fail("app: %q is a name quark uses for itself", m.App)
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
		case Web, Worker, Cron:
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
			fail("%s: kind is required: web, static, worker or cron", at)
		default:
			fail("%s: kind %q isn't one of web, static, worker, cron", at, w.Kind)
		}
		if w.Kind == Cron {
			if w.Schedule == "" {
				fail("%s: a cron workload needs schedule, five fields such as \"0 3 * * *\"", at)
			} else if _, err := ParseSchedule(w.Schedule); err != nil {
				fail("%s.schedule: %v", at, err)
			}
			if len(w.Routes) > 0 || w.Port != 0 {
				fail("%s: a cron workload has no routes or port", at)
			}
			if w.Timeout != "" {
				if _, err := time.ParseDuration(w.Timeout); err != nil {
					fail("%s.timeout: %q isn't a duration such as 30m", at, w.Timeout)
				}
			}
		} else if w.Schedule != "" || w.Timeout != "" {
			fail("%s: schedule and timeout are for cron workloads", at)
		}
		for i, mt := range w.Mounts {
			ma := fmt.Sprintf("%s.mounts[%d]", at, i)
			if m.Data == nil || m.Data.Volumes == nil {
				fail("%s: volume %q isn't declared under data.volumes", ma, mt.Volume)
			} else if _, ok := m.Data.Volumes[mt.Volume]; !ok {
				fail("%s: volume %q isn't declared under data.volumes", ma, mt.Volume)
			}
			if !strings.HasPrefix(mt.Path, "/") {
				fail("%s: path must be absolute", ma)
			}
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
			switch r.DNS {
			case DNSManual, DNSDirect, DNSProxied:
			default:
				fail("%s.dns: %q isn't direct or proxied", ra, r.DNS)
			}
		}
		if w.User != "" && !userPattern.MatchString(w.User) {
			fail("%s.user: %q must be a user, uid, or user:group", at, w.User)
		}
		for _, c := range w.Capabilities {
			if !capPattern.MatchString(strings.TrimPrefix(strings.ToUpper(c), "CAP_")) {
				fail("%s.capabilities: %q isn't a capability name such as NET_BIND_SERVICE", at, c)
			}
		}
		if w.Privileged && len(w.Capabilities) > 0 {
			fail("%s: a privileged workload keeps every capability already; drop capabilities", at)
		}
		for _, t := range w.Tmpfs {
			if !strings.HasPrefix(t, "/") {
				fail("%s.tmpfs: %q must be an absolute path", at, t)
			}
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
	modes := map[string]DNSMode{}
	for _, name := range m.WorkloadNames() {
		for _, r := range m.Workloads[name].Routes {
			if prev, seen := modes[r.Host]; seen && prev != r.DNS {
				fail("routes: %s has dns %q on one route and %q on another; use one", r.Host, prev, r.DNS)
			}
			modes[r.Host] = r.DNS
		}
	}
	if m.Data != nil {
		for name := range m.Data.Volumes {
			if !namePattern.MatchString(name) {
				fail("data.volumes: %q must be lowercase letters, digits and hyphens", name)
			}
			if name == DatabaseVolume {
				fail("data.volumes: %q is the database's own volume; pick another name", name)
			}
		}
		if m.Data.Postgres != nil {
			switch m.Data.Postgres.Version {
			case "", "15", "16", "17", "18":
			default:
				fail("data.postgres.version: %q isn't a supported major version (15 to 18)", m.Data.Postgres.Version)
			}
		}
	}
	if b := m.Backup; b != nil {
		if !m.HasData() && !b.Off {
			fail("backup: the app keeps no data to back up; declare data first")
		}
		if b.Schedule != "" {
			if _, err := ParseSchedule(b.Schedule); err != nil {
				fail("backup.schedule: %v", err)
			}
		}
		if b.Drill != "" {
			if _, err := ParseSchedule(b.Drill); err != nil {
				fail("backup.drill: %v", err)
			}
		}
		if k := b.Keep; k != nil {
			if k.Daily < 0 || k.Weekly < 0 || k.Monthly < 0 {
				fail("backup.keep: counts can't be negative")
			}
			if k.Daily+k.Weekly+k.Monthly == 0 {
				fail("backup.keep: keep at least one daily, weekly or monthly backup")
			}
		}
		if v := b.Verify; v != nil {
			if strings.TrimSpace(v.SQL) == "" {
				fail("backup.verify: sql is required")
			}
			if m.PostgresVersion() == "" {
				fail("backup.verify: a query needs a database; the app declares none")
			}
			if v.AtLeast < 0 {
				fail("backup.verify.at_least: can't be negative")
			}
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

// WorkloadFor finds the workload that serves a host and path: the route
// with the longest matching prefix wins, as it does at the edge.
func (m *Manifest) WorkloadFor(host, path string) (string, Workload, bool) {
	if path == "" {
		path = "/"
	}
	probe := strings.TrimSuffix(path, "/") + "/"
	var (
		bestName string
		bestLen  = -1
	)
	for _, name := range m.WorkloadNames() {
		for _, r := range m.Workloads[name].Routes {
			prefix := r.NormalizedPath()
			if r.Host == host && strings.HasPrefix(probe, prefix) && len(prefix) > bestLen {
				bestName, bestLen = name, len(prefix)
			}
		}
	}
	if bestLen < 0 {
		return "", Workload{}, false
	}
	return bestName, m.Workloads[bestName], true
}

// ParseSchedule parses a five-field cron schedule.
func ParseSchedule(spec string) (cron.Schedule, error) {
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec)
}

// PostgresVersion returns the app's database major version, or "" when it
// has no database.
func (m *Manifest) PostgresVersion() string {
	if m.Data == nil || m.Data.Postgres == nil {
		return ""
	}
	if m.Data.Postgres.Version == "" {
		return "16"
	}
	return m.Data.Postgres.Version
}

// Serves reports whether a workload listens for the edge.
func (w Workload) Serves() bool { return w.Kind == Web || w.Kind == Static }

// LongRunning reports whether a workload stays up between deploys.
func (w Workload) LongRunning() bool { return w.Kind != Cron }

// HasData reports whether the app keeps a database or volumes.
func (m *Manifest) HasData() bool {
	return m.Data != nil && (m.Data.Postgres != nil || len(m.Data.Volumes) > 0)
}

// BackedUp reports whether the app's data gets backed up.
func (m *Manifest) BackedUp() bool {
	return m.HasData() && (m.Backup == nil || !m.Backup.Off)
}

// BackupSchedule returns when backups run, with the default applied.
func (m *Manifest) BackupSchedule() string {
	if m.Backup != nil && m.Backup.Schedule != "" {
		return m.Backup.Schedule
	}
	return DefaultBackupSchedule
}

// DrillSchedule returns when drills run, with the default applied.
func (m *Manifest) DrillSchedule() string {
	if m.Backup != nil && m.Backup.Drill != "" {
		return m.Backup.Drill
	}
	return DefaultDrillSchedule
}

// KeepPolicy returns the retention, with the default applied.
func (m *Manifest) KeepPolicy() Keep {
	if m.Backup != nil && m.Backup.Keep != nil {
		return *m.Backup.Keep
	}
	return DefaultKeep
}

// VerifyQuery returns the drill's query and its threshold, or "" when
// the app declares none.
func (m *Manifest) VerifyQuery() (string, int) {
	if m.Backup == nil || m.Backup.Verify == nil {
		return "", 0
	}
	atLeast := m.Backup.Verify.AtLeast
	if atLeast == 0 {
		atLeast = 1
	}
	return m.Backup.Verify.SQL, atLeast
}

// ManagedHosts returns the hosts whose records quark keeps, with their
// mode, in host order. A host routed twice with different modes is an
// error at validation time, so the first wins here.
func (m *Manifest) ManagedHosts() map[string]DNSMode {
	out := map[string]DNSMode{}
	for _, name := range m.WorkloadNames() {
		for _, r := range m.Workloads[name].Routes {
			if r.DNS.Managed() {
				if _, seen := out[r.Host]; !seen {
					out[r.Host] = r.DNS
				}
			}
		}
	}
	return out
}

// Capabilities returns the capability names to keep, with CAP_ stripped.
func (w Workload) CapabilityNames() []string {
	var out []string
	for _, c := range w.Capabilities {
		out = append(out, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}
	return out
}
