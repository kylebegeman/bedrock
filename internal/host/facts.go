package host

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"
)

// Facts is what quark knows about the machine after looking, without
// changing anything.
type Facts struct {
	OSID      string `json:"os_id"`
	OSVersion string `json:"os_version"`
	OSName    string `json:"os_name"`
	Arch      string `json:"arch"`
	Hostname  string `json:"hostname"`
	CPUs      int    `json:"cpus"`
	// MemoryBytes and DiskFreeBytes are the machine's memory and the space
	// free on the root filesystem.
	MemoryBytes   uint64 `json:"memory_bytes"`
	DiskFreeBytes uint64 `json:"disk_free_bytes"`
	SwapBytes     uint64 `json:"swap_bytes"`
	Privileged    bool   `json:"privileged"`
	Systemd       bool   `json:"systemd"`

	Docker struct {
		Installed bool   `json:"installed"`
		Running   bool   `json:"running"`
		Version   string `json:"version,omitempty"`
		Compose   bool   `json:"compose"`
		Buildx    bool   `json:"buildx"`
	} `json:"docker"`
	Firewall struct {
		Installed bool     `json:"installed"`
		Active    bool     `json:"active"`
		Allowed   []string `json:"allowed,omitempty"`
	} `json:"firewall"`
	SSH struct {
		PasswordAuth bool   `json:"password_auth"`
		RootLogin    string `json:"root_login"`
		RootKeys     int    `json:"root_keys"`
	} `json:"ssh"`
	RebootRequired     bool   `json:"reboot_required"`
	UpdatesPending     int    `json:"updates_pending"`
	TimeSynced         bool   `json:"time_synced"`
	Timezone           string `json:"timezone,omitempty"`
	UnattendedUpgrades bool   `json:"unattended_upgrades"`
	JournalMaxUse      string `json:"journal_max_use,omitempty"`
	Fail2ban           bool   `json:"fail2ban"`
	Registry           struct {
		Present bool `json:"present"`
		Running bool `json:"running"`
	} `json:"registry"`
	EdgeRunning   bool `json:"edge_running"`
	DaemonAnswers bool `json:"daemon_answers"`
	// PushUser is whether the quark user, which receives pushes, exists.
	PushUser bool `json:"push_user"`
}

// RegistryContainer is the local image registry every build lands in.
const RegistryContainer = "quark-registry"

// Gather looks at the machine. Every probe tolerates its command being
// missing; the facts just stay zero.
func Gather(ctx context.Context, env Env, socket string) Facts {
	var f Facts
	f.Privileged = env.Privileged
	f.Systemd = env.Exists("/run/systemd/system")
	if release, err := env.ReadFile("/etc/os-release"); err == nil {
		kv := parseKeyValues(release)
		f.OSID, f.OSVersion, f.OSName = kv["ID"], kv["VERSION_ID"], kv["PRETTY_NAME"]
	}
	f.Arch, _ = env.Run(ctx, "uname", "-m")
	f.Hostname, _ = env.Run(ctx, "hostname")
	if out, err := env.Run(ctx, "nproc"); err == nil {
		f.CPUs, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if meminfo, err := env.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(meminfo, "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.ParseUint(fields[1], 10, 64)
					f.MemoryBytes = kb * 1024
				}
			}
		}
	}
	if out, err := env.Run(ctx, "df", "-Pk", "/"); err == nil {
		lines := strings.Split(out, "\n")
		if len(lines) >= 2 {
			if fields := strings.Fields(lines[1]); len(fields) >= 4 {
				kb, _ := strconv.ParseUint(fields[3], 10, 64)
				f.DiskFreeBytes = kb * 1024
			}
		}
	}
	if out, err := env.Run(ctx, "swapon", "--show=SIZE", "--noheadings", "--bytes"); err == nil {
		for _, line := range strings.Fields(out) {
			n, _ := strconv.ParseUint(line, 10, 64)
			f.SwapBytes += n
		}
	}

	if _, err := env.Run(ctx, "docker", "--version"); err == nil {
		f.Docker.Installed = true
		if v, err := env.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err == nil && v != "" {
			f.Docker.Running, f.Docker.Version = true, v
		}
		_, err := env.Run(ctx, "docker", "compose", "version")
		f.Docker.Compose = err == nil
		_, err = env.Run(ctx, "docker", "buildx", "version")
		f.Docker.Buildx = err == nil
		if state, err := env.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", RegistryContainer); err == nil {
			f.Registry.Present = true
			f.Registry.Running = strings.TrimSpace(state) == "true"
		}
		if state, err := env.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", "quark-edge"); err == nil {
			f.EdgeRunning = strings.TrimSpace(state) == "true"
		}
	}

	if out, err := env.Run(ctx, "ufw", "status"); err == nil {
		f.Firewall.Installed = true
		f.Firewall.Active = strings.Contains(out, "Status: active")
		f.Firewall.Allowed = parseUFWAllowed(out)
	}

	if out, err := env.Run(ctx, "sshd", "-T"); err == nil {
		kv := parseSSHDConfig(out)
		f.SSH.PasswordAuth = kv["passwordauthentication"] != "no"
		f.SSH.RootLogin = kv["permitrootlogin"]
	}
	if keys, err := env.ReadFile("/root/.ssh/authorized_keys"); err == nil {
		for _, line := range strings.Split(keys, "\n") {
			if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
				f.SSH.RootKeys++
			}
		}
	}

	f.RebootRequired = env.Exists("/var/run/reboot-required")
	f.UpdatesPending = -1
	if out, err := env.Run(ctx, "apt-get", "-s", "-o", "Debug::NoLocking=true", "upgrade"); err == nil {
		f.UpdatesPending = 0
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "Inst ") {
				f.UpdatesPending++
			}
		}
	}
	if out, err := env.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		f.TimeSynced = strings.TrimSpace(out) == "yes"
	}
	f.Timezone, _ = env.Run(ctx, "timedatectl", "show", "-p", "Timezone", "--value")
	if conf, err := env.ReadFile("/etc/apt/apt.conf.d/20auto-upgrades"); err == nil {
		f.UnattendedUpgrades = strings.Contains(conf, `Unattended-Upgrade "1"`)
	}
	if conf, err := env.ReadFile("/etc/systemd/journald.conf.d/quark.conf"); err == nil {
		f.JournalMaxUse = parseKeyValues(conf)["SystemMaxUse"]
	}
	if _, err := env.Run(ctx, "id", "-u", "quark"); err == nil {
		f.PushUser = true
	}
	if out, err := env.Run(ctx, "systemctl", "is-active", "fail2ban"); err == nil {
		f.Fail2ban = strings.TrimSpace(out) == "active"
	}
	if socket != "" {
		if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
			conn.Close()
			f.DaemonAnswers = true
		}
	}
	return f
}

// parseKeyValues reads KEY=value lines, unquoting values.
func parseKeyValues(text string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return kv
}

func parseSSHDConfig(text string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			kv[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// parseUFWAllowed pulls the allowed rules' targets from `ufw status`.
func parseUFWAllowed(text string) []string {
	var allowed []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == "ALLOW" && !strings.Contains(fields[0], "(v6)") {
			if !seen[fields[0]] {
				seen[fields[0]] = true
				allowed = append(allowed, fields[0])
			}
		}
	}
	return allowed
}

// Allows reports whether the firewall allows a port such as "22/tcp",
// including through a named application profile like OpenSSH.
func (f Facts) Allows(port string) bool {
	for _, rule := range f.Firewall.Allowed {
		if rule == port || (port == "22/tcp" && (rule == "OpenSSH" || rule == "22")) || rule == strings.TrimSuffix(port, "/tcp") {
			return true
		}
	}
	return false
}

// Addresses returns the machine's global addresses, the ones DNS should
// point at.
func Addresses(ctx context.Context, env Env) []string {
	out, err := env.Run(ctx, "ip", "-o", "addr", "show", "scope", "global")
	if err != nil {
		return nil
	}
	var addrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && (fields[2] == "inet" || fields[2] == "inet6") {
			addr := strings.SplitN(fields[3], "/", 2)[0]
			if !strings.HasPrefix(addr, "172.") && !strings.HasPrefix(addr, "192.168.") && !strings.HasPrefix(addr, "10.") {
				addrs = append(addrs, addr)
			}
		}
	}
	return addrs
}
