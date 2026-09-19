package host

import (
	"fmt"
)

// Verdict is how a check came out.
type Verdict string

const (
	Pass Verdict = "pass"
	Warn Verdict = "warn"
	Fail Verdict = "fail"
)

// Result is one check's outcome, with the fix when there is one.
type Result struct {
	Name    string  `json:"name"`
	Verdict Verdict `json:"verdict"`
	Detail  string  `json:"detail"`
	Fix     string  `json:"fix,omitempty"`
}

const (
	gib = 1 << 30
	gb  = 1_000_000_000
)

var supportedSystems = map[string][]string{
	"ubuntu": {"22.04", "24.04"},
	"debian": {"12", "13"},
}

// Supported reports whether quark runs on this OS and architecture.
func Supported(f Facts) bool {
	if f.Arch != "x86_64" && f.Arch != "aarch64" {
		return false
	}
	for _, v := range supportedSystems[f.OSID] {
		if v == f.OSVersion {
			return true
		}
	}
	return false
}

// Diagnose turns facts into verdicts. Fails block setup or mean something
// is broken; warns are worth fixing.
func Diagnose(f Facts) []Result {
	var out []Result
	add := func(name string, v Verdict, detail, fix string) {
		out = append(out, Result{Name: name, Verdict: v, Detail: detail, Fix: fix})
	}
	const setup = "run quark host setup"

	if Supported(f) {
		add("system", Pass, fmt.Sprintf("%s on %s", f.OSName, f.Arch), "")
	} else {
		add("system", Fail, fmt.Sprintf("%s on %s", orUnknown(f.OSName), orUnknown(f.Arch)), "quark needs Ubuntu 22.04 or 24.04, or Debian 12 or 13, on x86_64 or aarch64")
	}
	if !f.Systemd {
		add("systemd", Fail, "not running", "quark's daemon needs systemd")
	} else {
		add("systemd", Pass, "running", "")
	}
	if !f.Privileged {
		add("privileges", Warn, "not root", "setup and the daemon need root")
	} else {
		add("privileges", Pass, "root", "")
	}

	switch {
	case f.CPUs < 2:
		add("cpu", Fail, fmt.Sprintf("%d cpu", f.CPUs), "quark needs at least 2")
	case f.CPUs < 4:
		add("cpu", Warn, fmt.Sprintf("%d cpus", f.CPUs), "builds are slow below 4")
	default:
		add("cpu", Pass, fmt.Sprintf("%d cpus", f.CPUs), "")
	}
	switch {
	case f.MemoryBytes < 2*gib:
		add("memory", Fail, gigs(f.MemoryBytes), "quark needs at least 2 GiB")
	case f.MemoryBytes < 8*gib:
		add("memory", Warn, gigs(f.MemoryBytes), "apps and builds share memory; 8 GiB or more is comfortable")
	default:
		add("memory", Pass, gigs(f.MemoryBytes), "")
	}
	switch {
	case f.DiskFreeBytes < 5*gb:
		add("disk", Fail, gigs(f.DiskFreeBytes)+" free", "less than 5 GB free; free space or run quark gc")
	case f.DiskFreeBytes < 20*gb:
		add("disk", Warn, gigs(f.DiskFreeBytes)+" free", "less than 20 GB free; images and backups need room")
	default:
		add("disk", Pass, gigs(f.DiskFreeBytes)+" free", "")
	}
	if f.SwapBytes == 0 {
		add("swap", Warn, "none", setup+" adds a swap file")
	} else {
		add("swap", Pass, gigs(f.SwapBytes), "")
	}

	switch {
	case !f.Docker.Installed:
		add("docker", Fail, "not installed", setup)
	case !f.Docker.Running:
		add("docker", Fail, "installed but not running", "systemctl start docker")
	case !f.Docker.Compose || !f.Docker.Buildx:
		add("docker", Warn, "running, plugins missing", setup+" installs compose and buildx")
	default:
		add("docker", Pass, "running, "+f.Docker.Version, "")
	}
	switch {
	case !f.Registry.Present:
		add("registry", Fail, "no local image registry", setup)
	case !f.Registry.Running:
		add("registry", Fail, "registry container stopped", "docker start "+RegistryContainer)
	default:
		add("registry", Pass, "running on 127.0.0.1:5000", "")
	}

	if f.Docker.Running {
		if f.EdgeRunning {
			add("edge", Pass, "running on 80 and 443", "")
		} else {
			add("edge", Fail, "not running", setup)
		}
	}

	switch {
	case !f.Firewall.Installed:
		add("firewall", Fail, "ufw not installed", setup)
	case !f.Firewall.Active:
		add("firewall", Fail, "ufw inactive", setup)
	case !f.Allows("22/tcp"):
		add("firewall", Fail, "active without ssh allowed", "ufw allow OpenSSH, before anything else")
	case !f.Allows("80/tcp") || !f.Allows("443/tcp"):
		add("firewall", Warn, "active, web ports closed", setup+" opens 80 and 443")
	default:
		add("firewall", Pass, "active: ssh, 80, 443", "")
	}
	switch {
	case f.SSH.PasswordAuth && f.SSH.RootKeys == 0:
		add("ssh", Fail, "password login on, root has no key", "add a key to /root/.ssh/authorized_keys, then "+setup)
	case f.SSH.PasswordAuth:
		add("ssh", Fail, "password login on", setup+" turns it off")
	case f.SSH.RootKeys == 0:
		add("ssh", Warn, "key-only, but root has no key", "add a key to /root/.ssh/authorized_keys")
	default:
		add("ssh", Pass, fmt.Sprintf("key-only, %d root key(s)", f.SSH.RootKeys), "")
	}
	if !f.Fail2ban {
		add("fail2ban", Warn, "not active", setup)
	} else {
		add("fail2ban", Pass, "active", "")
	}

	switch {
	case f.RebootRequired:
		add("updates", Warn, "a reboot is pending", "quark host maintain")
	case f.UpdatesPending > 0:
		add("updates", Warn, fmt.Sprintf("%d package(s) can be upgraded", f.UpdatesPending), "quark host maintain")
	case f.UpdatesPending < 0:
		add("updates", Warn, "unknown", "apt-get didn't answer")
	default:
		add("updates", Pass, "up to date", "")
	}
	if !f.UnattendedUpgrades {
		add("security updates", Warn, "automatic security updates off", setup)
	} else {
		add("security updates", Pass, "automatic", "")
	}
	if !f.TimeSynced {
		add("time", Warn, "clock not synchronized", "systemctl restart systemd-timesyncd")
	} else {
		add("time", Pass, "synchronized"+tz(f.Timezone), "")
	}
	if f.JournalMaxUse == "" {
		add("logs", Warn, "journal unbounded", setup+" caps it")
	} else {
		add("logs", Pass, "journal capped at "+f.JournalMaxUse, "")
	}
	if f.Systemd {
		if !f.DaemonAnswers {
			add("daemon", Fail, "not answering", "quark daemon install")
		} else {
			add("daemon", Pass, "answering", "")
		}
	}
	return out
}

// Worst returns the worst verdict in a set.
func Worst(results []Result) Verdict {
	worst := Pass
	for _, r := range results {
		if r.Verdict == Fail {
			return Fail
		}
		if r.Verdict == Warn {
			worst = Warn
		}
	}
	return worst
}

func gigs(b uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(b)/gib)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func tz(z string) string {
	if z == "" {
		return ""
	}
	return ", " + z
}
