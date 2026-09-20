package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/kernel"
)

// SetupKind is the operation that turns a fresh machine into a bedrock host,
// and brings a drifted one back. Every step is safe to run again.
const SetupKind = "host.setup"

// Setup is the Definition for SetupKind.
type Setup struct {
	Env    Env
	Socket string
	// EnsureEdge runs the web edge; nil skips it (tests).
	EnsureEdge func(ctx context.Context, out io.Writer) error
}

// Kind implements kernel.Definition.
func (Setup) Kind() string { return SetupKind }

// Packages bedrock installs on every machine.
var basePackages = []string{"ca-certificates", "curl", "gnupg", "ufw", "fail2ban", "unattended-upgrades", "jq", "git"}

// Packages Docker's repository provides.
var dockerPackages = []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"}

// Files bedrock writes. Each is complete: bedrock owns them.
const (
	dockerDaemonJSON = `{
  "log-driver": "json-file",
  "log-opts": {"max-size": "20m", "max-file": "5"},
  "live-restore": true
}
`
	autoUpgrades = `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
APT::Periodic::AutocleanInterval "7";
`
	// Security updates install themselves; reboots wait for a maintenance
	// window, so apps come back in order and get checked.
	unattendedBedrock = `Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
`
	journaldBedrock = `[Journal]
SystemMaxUse=500M
`
	sshdBedrock = `# Written by bedrock host setup. Keys only.
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
`
	fail2banBedrock = `[sshd]
enabled = true
`
)

// RegistryImage is the local registry, pinned by digest.
const RegistryImage = "registry:3@sha256:fd374bae807c225661adfe2c0c1f9970a0b8fab1761fd7dfb91e0fd9a8748f9b"

// Plan implements kernel.Definition. It reads the machine to annotate each
// step and changes nothing.
func (s Setup) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	env := s.Env
	f := Gather(ctx, env, s.Socket)
	if !Supported(f) {
		return nil, fmt.Errorf("%s on %s isn't supported: bedrock needs Ubuntu 22.04 or 24.04, or Debian 12 or 13, on x86_64 or aarch64", orUnknown(f.OSName), orUnknown(f.Arch))
	}
	if !f.Privileged {
		return nil, errors.New("host setup needs root")
	}
	if !f.Systemd {
		return nil, errors.New("host setup needs systemd")
	}
	codename, debArch := osCodename(env), debianArch(ctx, env)
	installed := installedPackages(ctx, env, append(append([]string{}, basePackages...), dockerPackages...))
	missingBase := missing(installed, basePackages)
	missingDocker := missing(installed, dockerPackages)

	plan := &kernel.Plan{Target: p.Hostname, Recovery: kernel.Resume}
	add := func(st kernel.Step) { plan.Steps = append(plan.Steps, st) }

	add(kernel.Step{
		Name: "packages", Change: "install " + strings.Join(basePackages, ", "),
		Note: doneIf(len(missingBase) == 0, "all installed", "missing "+strings.Join(missingBase, ", ")),
		Apply: func(ctx context.Context, out io.Writer) error {
			if _, err := env.Run(ctx, "apt-get", "update"); err != nil {
				return err
			}
			_, err := env.Run(ctx, "apt-get", append([]string{"install", "-y", "--no-install-recommends"}, basePackages...)...)
			fmt.Fprintln(out, "packages present")
			return err
		},
	})
	add(kernel.Step{
		Name: "hostname", Change: "set the hostname to " + p.Hostname,
		Note: doneIf(f.Hostname == p.Hostname, "already "+p.Hostname, "currently "+orUnknown(f.Hostname)),
		Apply: func(ctx context.Context, out io.Writer) error {
			if _, err := env.Run(ctx, "hostnamectl", "set-hostname", p.Hostname); err != nil {
				return err
			}
			return ensureHostsEntry(env, p.Hostname, out)
		},
	})
	if p.Timezone != "" {
		add(kernel.Step{
			Name: "timezone", Change: "set the timezone to " + p.Timezone,
			Note: doneIf(f.Timezone == p.Timezone, "already "+p.Timezone, "currently "+orUnknown(f.Timezone)),
			Apply: func(ctx context.Context, _ io.Writer) error {
				_, err := env.Run(ctx, "timedatectl", "set-timezone", p.Timezone)
				return err
			},
		})
	}
	if p.SwapGiB > 0 {
		add(kernel.Step{
			Name: "swap", Change: fmt.Sprintf("keep a %d GiB swap file", p.SwapGiB),
			Note: doneIf(f.SwapBytes > 0, "already "+gigs(f.SwapBytes), "none yet"),
			Apply: func(ctx context.Context, out io.Writer) error {
				return ensureSwap(ctx, env, p.SwapGiB, out)
			},
		})
	}
	add(kernel.Step{
		Name: "docker", Change: "install Docker from Docker's repository, with compose and buildx",
		Note: doneIf(len(missingDocker) == 0 && f.Docker.Running, "already "+orUnknown(f.Docker.Version), "not installed"),
		Apply: func(ctx context.Context, out io.Writer) error {
			return ensureDocker(ctx, env, f.OSID, codename, debArch, len(missingDocker) > 0, out)
		},
	})
	add(kernel.Step{
		Name: "registry", Change: "run the local image registry on 127.0.0.1:5000",
		Note: doneIf(f.Registry.Running, "already running", "not running"),
		Apply: func(ctx context.Context, out io.Writer) error {
			return ensureRegistry(ctx, env, out)
		},
	})
	add(kernel.Step{
		Name: "edge", Change: "run the web edge on ports 80 and 443",
		Note: doneIf(f.EdgeRunning, "already running", "not running"),
		Apply: func(ctx context.Context, out io.Writer) error {
			if s.EnsureEdge == nil {
				return nil
			}
			return s.EnsureEdge(ctx, out)
		},
	})
	add(kernel.Step{
		Name: "security-updates", Change: "install security updates automatically, without automatic reboots",
		Note: doneIf(f.UnattendedUpgrades, "already on", "off"),
		Apply: func(ctx context.Context, out io.Writer) error {
			if _, err := env.WriteFile("/etc/apt/apt.conf.d/20auto-upgrades", autoUpgrades, 0o644); err != nil {
				return err
			}
			_, err := env.WriteFile("/etc/apt/apt.conf.d/52bedrock-unattended", unattendedBedrock, 0o644)
			return err
		},
	})
	add(kernel.Step{
		Name: "journal", Change: "cap the system journal at 500M",
		Note: doneIf(f.JournalMaxUse == "500M", "already capped", "unbounded"),
		Apply: func(ctx context.Context, _ io.Writer) error {
			changed, err := env.WriteFile("/etc/systemd/journald.conf.d/bedrock.conf", journaldBedrock, 0o644)
			if err != nil || !changed {
				return err
			}
			_, err = env.Run(ctx, "systemctl", "restart", "systemd-journald")
			return err
		},
	})
	add(kernel.Step{
		Name: "ssh", Change: "allow only key logins over SSH",
		Note: doneIf(!f.SSH.PasswordAuth, "already keys only", "password logins on"),
		Apply: func(ctx context.Context, out io.Writer) error {
			return ensureSSHKeysOnly(ctx, env, out)
		},
	})
	add(kernel.Step{
		Name: "firewall", Change: "allow only ssh, 80 and 443 in",
		Note: doneIf(f.Firewall.Active && f.Allows("22/tcp") && f.Allows("80/tcp") && f.Allows("443/tcp"), "already active", "inactive"),
		Apply: func(ctx context.Context, out io.Writer) error {
			return ensureFirewall(ctx, env, out)
		},
	})
	add(kernel.Step{
		Name: "fail2ban", Change: "ban repeated SSH failures",
		Note: doneIf(f.Fail2ban, "already active", "inactive"),
		Apply: func(ctx context.Context, _ io.Writer) error {
			if _, err := env.WriteFile("/etc/fail2ban/jail.d/bedrock.conf", fail2banBedrock, 0o644); err != nil {
				return err
			}
			_, err := env.Run(ctx, "systemctl", "enable", "--now", "fail2ban")
			if err != nil {
				return err
			}
			_, err = env.Run(ctx, "systemctl", "restart", "fail2ban")
			return err
		},
	})
	add(kernel.Step{
		Name: "pushes", Change: "keep the bedrock user, which receives git pushes; each of its keys deploys only the apps it names",
		Note: doneIf(f.PushUser, "already there", "no bedrock user yet"),
		Apply: func(ctx context.Context, out io.Writer) error {
			return ensurePushUser(ctx, env, s.Socket, out)
		},
	})
	add(kernel.Step{
		Name: "time", Change: "keep the clock synchronized",
		Note: doneIf(f.TimeSynced, "already synchronized", "not synchronized"),
		Apply: func(ctx context.Context, _ io.Writer) error {
			_, err := env.Run(ctx, "systemctl", "enable", "--now", "systemd-timesyncd")
			return err
		},
	})
	add(kernel.Step{
		Name: "profile", Change: "record the profile at " + ProfilePath,
		Apply: func(_ context.Context, _ io.Writer) error { return SaveProfile(env, p) },
	})
	return plan, nil
}

func doneIf(done bool, yes, no string) string {
	if done {
		return yes
	}
	return no
}

func osCodename(env Env) string {
	release, _ := env.ReadFile("/etc/os-release")
	return parseKeyValues(release)["VERSION_CODENAME"]
}

func debianArch(ctx context.Context, env Env) string {
	out, err := env.Run(ctx, "dpkg", "--print-architecture")
	if err != nil {
		return "amd64"
	}
	return strings.TrimSpace(out)
}

// installedPackages asks dpkg which of the packages are installed.
func installedPackages(ctx context.Context, env Env, packages []string) map[string]bool {
	installed := map[string]bool{}
	out, _ := env.Run(ctx, "dpkg-query", append([]string{"-W", "-f", "${binary:Package} ${Status}\n"}, packages...)...)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[len(fields)-1] == "installed" {
			installed[strings.SplitN(fields[0], ":", 2)[0]] = true
		}
	}
	return installed
}

func missing(installed map[string]bool, wanted []string) []string {
	var out []string
	for _, p := range wanted {
		if !installed[p] {
			out = append(out, p)
		}
	}
	return out
}

func ensureHostsEntry(env Env, hostname string, out io.Writer) error {
	hosts, _ := env.ReadFile("/etc/hosts")
	for _, line := range strings.Split(hosts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "127.0.1.1" && fields[1] == hostname {
			return nil
		}
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(hosts, "\n"), "\n") {
		if fields := strings.Fields(line); len(fields) >= 1 && fields[0] == "127.0.1.1" {
			continue
		}
		kept = append(kept, line)
	}
	kept = append(kept, "127.0.1.1 "+hostname)
	if _, err := env.WriteFile("/etc/hosts", strings.Join(kept, "\n")+"\n", 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "hosts entry for %s\n", hostname)
	return nil
}

func ensureSwap(ctx context.Context, env Env, gib int, out io.Writer) error {
	if active, _ := env.Run(ctx, "swapon", "--show=SIZE", "--noheadings", "--bytes"); strings.TrimSpace(active) != "" {
		fmt.Fprintln(out, "swap already active")
		return nil
	}
	if !env.Exists("/swapfile") {
		if _, err := env.Run(ctx, "fallocate", "-l", fmt.Sprintf("%dG", gib), "/swapfile"); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"chmod", "600", "/swapfile"}, {"mkswap", "/swapfile"}, {"swapon", "/swapfile"}} {
		if _, err := env.Run(ctx, args[0], args[1:]...); err != nil {
			return err
		}
	}
	fstab, _ := env.ReadFile("/etc/fstab")
	if !strings.Contains(fstab, "/swapfile") {
		if _, err := env.WriteFile("/etc/fstab", strings.TrimRight(fstab, "\n")+"\n/swapfile none swap sw 0 0\n", 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "%d GiB swap file active\n", gib)
	return nil
}

func ensureDocker(ctx context.Context, env Env, osID, codename, debArch string, install bool, out io.Writer) error {
	if install {
		if _, err := env.Run(ctx, "install", "-m", "0755", "-d", "/etc/apt/keyrings"); err != nil {
			return err
		}
		if _, err := env.Run(ctx, "curl", "-fsSL", "https://download.docker.com/linux/"+osID+"/gpg", "-o", "/etc/apt/keyrings/docker.asc"); err != nil {
			return err
		}
		if _, err := env.Run(ctx, "chmod", "a+r", "/etc/apt/keyrings/docker.asc"); err != nil {
			return err
		}
		list := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/%s %s stable\n", debArch, osID, codename)
		if _, err := env.WriteFile("/etc/apt/sources.list.d/docker.list", list, 0o644); err != nil {
			return err
		}
		if _, err := env.Run(ctx, "apt-get", "update"); err != nil {
			return err
		}
		if _, err := env.Run(ctx, "apt-get", append([]string{"install", "-y"}, dockerPackages...)...); err != nil {
			return err
		}
		fmt.Fprintln(out, "docker installed")
	}
	changed, err := env.WriteFile("/etc/docker/daemon.json", dockerDaemonJSON, 0o644)
	if err != nil {
		return err
	}
	if _, err := env.Run(ctx, "systemctl", "enable", "--now", "docker"); err != nil {
		return err
	}
	if changed {
		if _, err := env.Run(ctx, "systemctl", "restart", "docker"); err != nil {
			return err
		}
		fmt.Fprintln(out, "docker configured: bounded logs, live restore")
	}
	return nil
}

func ensureRegistry(ctx context.Context, env Env, out io.Writer) error {
	state, err := env.Run(ctx, "docker", "inspect", "-f", "{{.State.Running}}", RegistryContainer)
	switch {
	case err == nil && strings.TrimSpace(state) == "true":
		return nil
	case err == nil:
		_, err := env.Run(ctx, "docker", "start", RegistryContainer)
		return err
	}
	_, err = env.Run(ctx, "docker", "run", "-d", "--name", RegistryContainer, "--restart", "unless-stopped",
		"--label", "bedrock.owner=bedrock", "-p", "127.0.0.1:5000:5000", "-v", "bedrock-registry:/var/lib/registry",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true", RegistryImage)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "registry started")
	return nil
}

// SSHDropIn is bedrock's sshd configuration. sshd keeps the first value it
// reads and reads sshd_config.d in name order, so the name sorts before
// anything cloud-init or an image leaves there.
const SSHDropIn = "/etc/ssh/sshd_config.d/00-bedrock.conf"

// ensureSSHKeysOnly turns password logins off, but never before root has a
// key to get back in with.
func ensureSSHKeysOnly(ctx context.Context, env Env, out io.Writer) error {
	keys, _ := env.ReadFile("/root/.ssh/authorized_keys")
	if strings.TrimSpace(keys) == "" {
		return errors.New("root has no SSH key in /root/.ssh/authorized_keys; turning passwords off would lock you out")
	}
	changed, err := env.WriteFile(SSHDropIn, sshdBedrock, 0o644)
	if err != nil {
		return err
	}
	// An earlier bedrock wrote a name that lost to cloud-init's drop-in.
	if stale := env.Path("/etc/ssh/sshd_config.d/50-bedrock.conf"); env.Exists("/etc/ssh/sshd_config.d/50-bedrock.conf") {
		_ = os.Remove(stale)
		changed = true
	}
	if !changed {
		return nil
	}
	if _, err := env.Run(ctx, "sshd", "-t"); err != nil {
		return fmt.Errorf("sshd rejected the new config: %w", err)
	}
	if _, err := env.Run(ctx, "systemctl", "reload", "ssh"); err != nil {
		if _, err2 := env.Run(ctx, "systemctl", "reload", "sshd"); err2 != nil {
			return err
		}
	}
	fmt.Fprintln(out, "password logins off")
	return nil
}

// ensureFirewall allows ssh before anything is denied, so a mistake can't
// lock the door.
func ensureFirewall(ctx context.Context, env Env, out io.Writer) error {
	for _, args := range [][]string{
		{"allow", "OpenSSH"}, {"allow", "22/tcp"}, {"allow", "80/tcp"}, {"allow", "443/tcp"},
		{"default", "deny", "incoming"}, {"default", "allow", "outgoing"}, {"--force", "enable"},
	} {
		if _, err := env.Run(ctx, "ufw", args...); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "firewall active: ssh, 80, 443")
	return nil
}

// ensurePushUser keeps the system user pushes arrive as: no password, a
// home that only it and root read, and a key file bedrock writes.
func ensurePushUser(ctx context.Context, env Env, socket string, out io.Writer) error {
	if _, err := env.Run(ctx, "id", "-u", gitdeploy.User); err != nil {
		if _, err := env.Run(ctx, "useradd", "--system", "--user-group", "--create-home", "--home-dir", gitdeploy.Home, "--shell", "/bin/sh", "--comment", "bedrock receives git pushes", gitdeploy.User); err != nil {
			return err
		}
		fmt.Fprintln(out, "bedrock user made")
	}
	for dir, mode := range map[string]string{gitdeploy.Home: "0750", gitdeploy.Home + "/.ssh": "0700", gitdeploy.BuildsDir: "0750"} {
		if _, err := env.Run(ctx, "install", "-d", "-o", gitdeploy.User, "-g", gitdeploy.User, "-m", mode, dir); err != nil {
			return err
		}
	}
	if !env.Exists(gitdeploy.AuthorizedKeys) {
		if _, err := env.Run(ctx, "install", "-o", gitdeploy.User, "-g", gitdeploy.User, "-m", "0600", "/dev/null", gitdeploy.AuthorizedKeys); err != nil {
			return err
		}
	}
	// A push asks the daemon to deploy through its socket, which the
	// daemon shares with the bedrock group when it starts; one already
	// running is given to the group here.
	if socket != "" && env.Exists(socket) {
		for _, args := range [][]string{{"chgrp", gitdeploy.User, filepath.Dir(socket)}, {"chmod", "0750", filepath.Dir(socket)}, {"chgrp", gitdeploy.User, socket}} {
			if _, err := env.Run(ctx, args[0], args[1:]...); err != nil {
				return err
			}
		}
	}
	return nil
}
