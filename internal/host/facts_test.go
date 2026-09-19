package host

import (
	"context"
	"testing"
)

func TestGatherReadsAFreshBox(t *testing.T) {
	m := freshUbuntu(t)
	f := Gather(context.Background(), m.env(), "")
	if f.OSID != "ubuntu" || f.OSVersion != "24.04" || f.Arch != "x86_64" || f.CPUs != 4 || !f.Systemd || !f.Privileged {
		t.Fatalf("basics: %+v", f)
	}
	if f.MemoryBytes != 16384000*1024 || f.DiskFreeBytes != 190000000*1024 || f.SwapBytes != 0 {
		t.Fatalf("sizes: mem=%d disk=%d swap=%d", f.MemoryBytes, f.DiskFreeBytes, f.SwapBytes)
	}
	if f.Docker.Installed || f.Registry.Present || f.Firewall.Active || f.Fail2ban {
		t.Fatalf("nothing should be installed yet: %+v", f)
	}
	if !f.SSH.PasswordAuth || f.SSH.RootLogin != "yes" || f.SSH.RootKeys != 1 {
		t.Fatalf("ssh: %+v", f.SSH)
	}
	if f.UpdatesPending != 2 || f.RebootRequired || !f.TimeSynced || f.Timezone != "Etc/UTC" {
		t.Fatalf("updates/time: %+v", f)
	}
	if f.UnattendedUpgrades || f.JournalMaxUse != "" || f.DaemonAnswers {
		t.Fatalf("config: %+v", f)
	}
}

func TestGatherReadsASetUpBox(t *testing.T) {
	m := setUpBox(t)
	m.write("/var/run/reboot-required", "")
	f := Gather(context.Background(), m.env(), "")
	if !f.Docker.Running || f.Docker.Version != "29.5.2" || !f.Docker.Compose || !f.Docker.Buildx {
		t.Fatalf("docker: %+v", f.Docker)
	}
	if !f.Registry.Present || !f.Registry.Running {
		t.Fatalf("registry: %+v", f.Registry)
	}
	if !f.Firewall.Active || !f.Allows("22/tcp") || !f.Allows("80/tcp") || !f.Allows("443/tcp") || f.Allows("5432/tcp") {
		t.Fatalf("firewall: %+v", f.Firewall)
	}
	if f.SSH.PasswordAuth || f.SSH.RootLogin != "prohibit-password" {
		t.Fatalf("ssh: %+v", f.SSH)
	}
	if f.SwapBytes != 4*gib || !f.Fail2ban || !f.UnattendedUpgrades || f.JournalMaxUse != "500M" || f.UpdatesPending != 0 || !f.RebootRequired {
		t.Fatalf("rest: %+v", f)
	}
}

func TestSupportedSystems(t *testing.T) {
	cases := []struct {
		id, version, arch string
		want              bool
	}{
		{"ubuntu", "24.04", "x86_64", true},
		{"ubuntu", "22.04", "aarch64", true},
		{"debian", "13", "x86_64", true},
		{"ubuntu", "20.04", "x86_64", false},
		{"debian", "11", "x86_64", false},
		{"ubuntu", "24.04", "armv7l", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		f := Facts{OSID: c.id, OSVersion: c.version, Arch: c.arch}
		if got := Supported(f); got != c.want {
			t.Errorf("%s %s %s: got %v want %v", c.id, c.version, c.arch, got, c.want)
		}
	}
}
