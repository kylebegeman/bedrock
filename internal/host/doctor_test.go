package host

import (
	"context"
	"testing"
)

func byName(results []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range results {
		m[r.Name] = r
	}
	return m
}

func TestAFreshBoxFailsTheDoctorWithSetupAsTheFix(t *testing.T) {
	f := Gather(context.Background(), freshUbuntu(t).env(), "")
	results := Diagnose(f)
	if Worst(results) != Fail {
		t.Fatalf("a fresh box must fail: %+v", results)
	}
	r := byName(results)
	for _, name := range []string{"docker", "registry", "firewall", "ssh", "daemon"} {
		if r[name].Verdict != Fail {
			t.Errorf("%s: got %s (%s), want fail", name, r[name].Verdict, r[name].Detail)
		}
	}
	for _, name := range []string{"swap", "fail2ban", "updates", "security updates", "logs"} {
		if r[name].Verdict != Warn {
			t.Errorf("%s: got %s (%s), want warn", name, r[name].Verdict, r[name].Detail)
		}
	}
	for _, name := range []string{"system", "systemd", "privileges", "cpu", "disk", "time"} {
		if r[name].Verdict != Pass {
			t.Errorf("%s: got %s (%s), want pass", name, r[name].Verdict, r[name].Detail)
		}
	}
	if r["docker"].Fix != "run quark host setup" || r["ssh"].Detail != "password login on" {
		t.Fatalf("fix text: %+v %+v", r["docker"], r["ssh"])
	}
}

func TestASetUpBoxPassesExceptWhatOnlyTimeChanges(t *testing.T) {
	m := setUpBox(t)
	f := Gather(context.Background(), m.env(), "")
	f.DaemonAnswers = true
	results := Diagnose(f)
	r := byName(results)
	for _, res := range results {
		switch res.Name {
		case "memory":
			// 15.6 GiB is above the 8 GiB line.
			if res.Verdict != Pass {
				t.Errorf("memory: %+v", res)
			}
		default:
			if res.Verdict != Pass {
				t.Errorf("%s: got %s (%s)", res.Name, res.Verdict, res.Detail)
			}
		}
	}
	if Worst(results) != Pass {
		t.Fatalf("worst: %s", Worst(results))
	}
	if r["firewall"].Detail != "active: ssh, 80, 443" || r["ssh"].Detail != "key-only, 1 root key(s)" {
		t.Fatalf("details: %+v %+v", r["firewall"], r["ssh"])
	}
}

func TestDoctorWarnsAboutPendingReboots(t *testing.T) {
	m := setUpBox(t)
	m.write("/var/run/reboot-required", "")
	f := Gather(context.Background(), m.env(), "")
	f.DaemonAnswers = true
	r := byName(Diagnose(f))
	if r["updates"].Verdict != Warn || r["updates"].Fix != "quark host maintain" {
		t.Fatalf("updates: %+v", r["updates"])
	}
}

func TestDoctorNeverSuggestsLockingYourselfOut(t *testing.T) {
	f := Facts{Firewall: struct {
		Installed bool     `json:"installed"`
		Active    bool     `json:"active"`
		Allowed   []string `json:"allowed,omitempty"`
	}{Installed: true, Active: true, Allowed: []string{"80/tcp"}}}
	r := byName(Diagnose(f))
	if r["firewall"].Verdict != Fail || r["firewall"].Fix != "ufw allow OpenSSH, before anything else" {
		t.Fatalf("firewall: %+v", r["firewall"])
	}
	f.SSH.PasswordAuth = true
	f.SSH.RootKeys = 0
	r = byName(Diagnose(f))
	if r["ssh"].Verdict != Fail || r["ssh"].Detail != "password login on, root has no key" {
		t.Fatalf("ssh: %+v", r["ssh"])
	}
}
