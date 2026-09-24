package watch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/edge"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

// What the prober considers wrong.
const (
	// DiskFreeMin is the least free space on the root filesystem before
	// the disk is a problem; DiskFreePercentMin the same, as a share.
	DiskFreeMin        = 5 << 30
	DiskFreePercentMin = 10.0
	// MemoryAvailableMin is the smallest share of memory that may be
	// available.
	MemoryAvailableMin = 5.0
	// CertificateWarn is how close to expiry a certificate may get.
	CertificateWarn = 14 * 24 * time.Hour
	// BackupStale is how old the last good backup may be.
	BackupStale = 36 * time.Hour
	// DataCheckEvery is how often a workload's volumes are walked for
	// files it can't write. A walk reads every entry, so rounds between
	// walks reuse the last answer.
	DataCheckEvery = time.Hour
	// EdgeAddress is where the machine's own edge answers TLS.
	EdgeAddress = "127.0.0.1:443"
)

// Prober looks at the machine and its apps and reports what is wrong.
type Prober struct {
	Store *state.Store
	Now   func() time.Time
	// Connect opens Docker; the default is the real one.
	Connect func(ctx context.Context) (*docker.Engine, error)
	// Root is the filesystem whose free space matters.
	Root string

	// mu keeps rounds from overlapping.
	mu sync.Mutex
	// restartsMu guards restarts, which one goroutine per app writes in a
	// round.
	restartsMu sync.Mutex
	restarts   map[string]int
	// dataMu guards data, the last walk of each container's volumes.
	dataMu sync.Mutex
	data   map[string]dataScan
}

// dataScan is one walk of one volume a container mounts.
type dataScan struct {
	at      time.Time
	problem string
}

// NewProber returns a prober for this machine.
func NewProber(store *state.Store) *Prober {
	return &Prober{Store: store, Now: func() time.Time { return time.Now().UTC() }, Connect: docker.Connect, Root: "/", restarts: map[string]int{}}
}

// Observe takes one look at everything and returns the conditions that
// hold right now. The host, each app and each watched URL get at most
// one condition each.
func (p *Prober) Observe(ctx context.Context) []Condition {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.restarts == nil {
		p.restarts = map[string]int{}
	}
	p.forgetOldScans(p.Now())
	var conditions []Condition
	conditions = append(conditions, p.host()...)

	e, err := p.Connect(ctx)
	if err != nil {
		conditions = append(conditions, Condition{Key: "host:docker", Subject: "docker", Severity: state.SeverityCritical, Message: "docker isn't answering: " + err.Error()})
		return append(conditions, p.watches(ctx)...)
	}
	defer e.Close()
	if info, err := e.Inspect(ctx, edge.Container); err != nil || !info.Running {
		conditions = append(conditions, Condition{Key: "host:edge", Subject: "the edge", Severity: state.SeverityCritical, Message: "the edge (bedrock-edge) isn't running; no site is reachable"})
	}

	active, err := p.Store.ActiveRevisions(ctx)
	if err != nil {
		return append(conditions, Condition{Key: "host:state", Subject: "the state store", Severity: state.SeverityCritical, Message: err.Error()})
	}
	type finding struct {
		app      string
		severity string
		problems []string
	}
	findings := make([]finding, len(active))
	var wg sync.WaitGroup
	for i, rev := range active {
		wg.Add(1)
		go func(i int, rev state.Revision) {
			defer wg.Done()
			f := finding{app: rev.App}
			f.severity, f.problems = p.app(ctx, e, rev)
			findings[i] = f
		}(i, rev)
	}
	wg.Wait()
	for _, f := range findings {
		if len(f.problems) > 0 {
			conditions = append(conditions, Condition{Key: "app:" + f.app, Subject: f.app, Severity: f.severity, Message: strings.Join(f.problems, "; ")})
		}
	}
	return append(conditions, p.watches(ctx)...)
}

// app looks at one app's containers, health, checks, certificates and
// backups and returns the worst severity and every problem.
func (p *Prober) app(ctx context.Context, e *docker.Engine, rev state.Revision) (string, []string) {
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return state.SeverityWarning, []string{"its manifest can't be read: " + err.Error()}
	}
	now := p.Now()
	var problems []string
	severity := ""
	worse := func(s string) {
		if severity == "" || s == state.SeverityCritical {
			severity = s
		}
	}
	for _, name := range m.WorkloadNames() {
		w := m.Workloads[name]
		if !w.LongRunning() {
			continue
		}
		container := rev.Containers[name]
		info, err := e.Inspect(ctx, container)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s has no container", name))
			worse(state.SeverityCritical)
			continue
		case !info.Running && w.Singleton && p.deploying(ctx, rev.App):
			// A deploy stopped it to start its successor.
			continue
		case !info.Running:
			since := ""
			if !info.FinishedAt.IsZero() {
				since = " " + ago(info.FinishedAt, now) + " ago"
			}
			problems = append(problems, fmt.Sprintf("%s isn't running (%s%s, exit code %d)", name, info.Status, since, info.ExitCode))
			worse(state.SeverityCritical)
			continue
		}
		p.restartsMu.Lock()
		last, seen := p.restarts[container]
		p.restarts[container] = info.Restarts
		p.restartsMu.Unlock()
		if seen && info.Restarts > last {
			problems = append(problems, fmt.Sprintf("%s keeps restarting (%d restarts)", name, info.Restarts))
			worse(state.SeverityCritical)
		}
		if w.Health != nil && len(w.Health.Command) > 0 {
			if err := app.HealthCommand(ctx, e, container, w.Health.Command); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				worse(state.SeverityCritical)
			}
		} else if w.Health != nil && w.Health.Path != "" && w.Serves() {
			if why := healthProblem(ctx, info, w); why != "" {
				problems = append(problems, fmt.Sprintf("%s's %s %s", name, w.Health.Path, why))
				worse(state.SeverityCritical)
			}
		}
		// A workload that answers can still be unable to save anything.
		for _, why := range p.dataProblems(rev.App, name, info, now) {
			problems = append(problems, why)
			worse(state.SeverityCritical)
		}
	}
	for _, c := range m.Checks {
		if err := app.RunCheck(ctx, c); err != nil {
			problems = append(problems, strings.TrimPrefix(err.Error(), "check "))
			worse(state.SeverityCritical)
		}
	}
	for _, host := range m.Hosts() {
		ready, why, expiry, found := edge.Certificate(ctx, host, EdgeAddress)
		if !found {
			continue // the edge condition covers a dead edge
		}
		if !ready {
			problems = append(problems, fmt.Sprintf("%s has no valid certificate: %s", host, why))
			worse(state.SeverityCritical)
			continue
		}
		if left := expiry.Sub(now); left < CertificateWarn {
			problems = append(problems, fmt.Sprintf("the certificate for %s expires in %d days and hasn't renewed", host, int(left.Hours()/24)))
			worse(state.SeverityWarning)
		}
	}
	if m.BackedUp() {
		if why := p.backupProblem(ctx, rev.App, now); why != "" {
			problems = append(problems, why)
			worse(state.SeverityWarning)
		}
		if last, err := p.Store.LastBackupRun(ctx, rev.App, state.BackupRunDrill); err == nil && last != nil && last.Finished() && !last.OK {
			problems = append(problems, "the last restore drill failed: "+last.Error)
			worse(state.SeverityWarning)
		}
	}
	return severity, problems
}

// dataProblems walks the volumes a running workload mounts for files and
// directories its user can't write, at most once per DataCheckEvery for
// each volume. Root, or a user bedrock didn't set, needs no walk; a
// volume that can't be walked says nothing rather than something wrong.
func (p *Prober) dataProblems(appName, workload string, info *docker.Info, now time.Time) []string {
	u, ok := docker.ParseUser(info.User)
	if !ok || u.Root() {
		return nil
	}
	var out []string
	for _, v := range info.Volumes {
		if !v.RW || v.Source == "" {
			continue
		}
		key := info.Name + " " + v.Name
		p.dataMu.Lock()
		scan, seen := p.data[key]
		p.dataMu.Unlock()
		if !seen || now.Sub(scan.at) >= DataCheckEvery {
			scan = dataScan{at: now}
			if x, err := docker.UnwritableBy(v.Source, u); err == nil {
				scan.problem = x.Problem(workload, u, docker.AppVolume(appName, v.Name))
			}
			p.dataMu.Lock()
			if p.data == nil {
				p.data = map[string]dataScan{}
			}
			p.data[key] = scan
			p.dataMu.Unlock()
		}
		if scan.problem != "" {
			out = append(out, scan.problem+"; bedrock doctor says how to give them back")
		}
	}
	return out
}

// forgetOldScans drops walks of containers that are gone, which stop
// being refreshed once a deploy replaces them.
func (p *Prober) forgetOldScans(now time.Time) {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	for key, scan := range p.data {
		if now.Sub(scan.at) > 2*DataCheckEvery {
			delete(p.data, key)
		}
	}
}

// deploying reports whether a deploy or rollback of the app is under way.
func (p *Prober) deploying(ctx context.Context, appName string) bool {
	ops, err := p.Store.List(ctx, 20)
	if err != nil {
		return false
	}
	for _, op := range ops {
		// The target is the app; before 0.7.8 it was "app revision".
		if (op.Kind == app.DeployKind || op.Kind == app.RollbackKind) && !op.Status.Final() && (op.Target == appName || strings.HasPrefix(op.Target, appName+" ")) {
			return true
		}
	}
	return false
}

// backupProblem says when an app's last good backup is too old, giving a
// new app time for its first one.
func (p *Prober) backupProblem(ctx context.Context, appName string, now time.Time) string {
	last, err := p.Store.LastGoodBackupRun(ctx, appName, state.BackupRunBackup)
	if err != nil {
		return ""
	}
	if last != nil {
		if age := now.Sub(last.StartedAt); age > BackupStale {
			return fmt.Sprintf("no successful backup for %s (the last was %s)", ago(last.StartedAt, now), last.StartedAt.Format("2006-01-02 15:04"))
		}
		return ""
	}
	revs, err := p.Store.Revisions(ctx, appName)
	if err != nil || len(revs) == 0 {
		return ""
	}
	first := revs[len(revs)-1].CreatedAt
	if now.Sub(first) > BackupStale {
		return fmt.Sprintf("never backed up, deployed %s ago", ago(first, now))
	}
	return ""
}

// healthProblem asks a workload's health path directly.
func healthProblem(ctx context.Context, info *docker.Info, w manifest.Workload) string {
	ip := info.IPs[docker.EdgeNetwork]
	if ip == "" {
		for _, v := range info.IPs {
			ip = v
		}
	}
	if ip == "" {
		return "can't be reached: no address"
	}
	port := w.Port
	if w.Kind == manifest.Static {
		port, _ = strconv.Atoi(docker.StaticPort)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d%s", ip, port, w.Health.Path), nil)
	if err != nil {
		return err.Error()
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "doesn't answer: " + trimURLError(err.Error())
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "answers " + resp.Status
	}
	return ""
}

func trimURLError(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

// watches checks the URLs this machine watches from the outside.
func (p *Prober) watches(ctx context.Context) []Condition {
	watches, err := p.Store.Watches(ctx)
	if err != nil || len(watches) == 0 {
		return nil
	}
	conditions := make([]Condition, len(watches))
	var wg sync.WaitGroup
	for i, w := range watches {
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			if err := app.RunCheck(ctx, manifest.Check{URL: url}); err != nil {
				conditions[i] = Condition{Key: "watch:" + url, Subject: url, Severity: state.SeverityCritical, Message: strings.TrimPrefix(err.Error(), "check ")}
			}
		}(i, w.URL)
	}
	wg.Wait()
	var out []Condition
	for _, c := range conditions {
		if c.Key != "" {
			out = append(out, c)
		}
	}
	return out
}

// host looks at disk and memory.
func (p *Prober) host() []Condition {
	var out []Condition
	if free, total, ok := DiskFree(p.Root); ok {
		pct := float64(free) / float64(total) * 100
		if free < DiskFreeMin || pct < DiskFreePercentMin {
			out = append(out, Condition{Key: "host:disk", Subject: "disk", Severity: state.SeverityCritical, Message: fmt.Sprintf("only %s free of %s (%.0f%%) on %s", app.HumanBytes(int64(free)), app.HumanBytes(int64(total)), pct, p.Root)})
		}
	}
	if available, total, ok := Memory(); ok {
		pct := float64(available) / float64(total) * 100
		if pct < MemoryAvailableMin {
			out = append(out, Condition{Key: "host:memory", Subject: "memory", Severity: state.SeverityWarning, Message: fmt.Sprintf("only %s of %s available (%.0f%%)", app.HumanBytes(int64(available)), app.HumanBytes(int64(total)), pct)})
		}
	}
	return out
}

// DiskFree reports free and total bytes on a filesystem.
func DiskFree(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), true
}

// Memory reads available and total memory from /proc/meminfo.
func Memory() (available, total uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return available, total, total > 0
}
