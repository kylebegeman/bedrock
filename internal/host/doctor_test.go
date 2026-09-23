package host

import (
	"context"
	"slices"
	"strings"
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
	if r["docker"].Fix != "run bedrock host setup" || r["ssh"].Detail != "password login on" {
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
	if r["updates"].Verdict != Warn || r["updates"].Fix != "bedrock host maintain" {
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

// Docker publishes a port ahead of ufw, so the doctor is where a port that
// is open without the firewall saying so shows up.
func TestThePortsAppsPublishAreListedAgainstTheFirewall(t *testing.T) {
	m := setUpBox(t)
	m.answers["docker ps --filter label=bedrock.app --format "+publishedFormat] = strings.Join([]string{
		"headscale\tserver\t0.0.0.0:3478->3478/udp, [::]:3478->3478/udp, 127.0.0.1:19090->9090/tcp, 8080/tcp",
		"relay\tstun\t0.0.0.0:3479->3479/udp, [::]:3479->3479/udp",
		"site\tweb\t8000/tcp",
	}, "\n")
	m.answers["ufw status"] += "3479/udp                   ALLOW       Anywhere\n"
	f := Gather(context.Background(), m.env(), "")
	want := []PublishedPort{{Port: "3478/udp", App: "headscale", Workload: "server"}, {Port: "3479/udp", App: "relay", Workload: "stun"}}
	if !slices.Equal(f.Published, want) {
		t.Fatalf("published %+v, want %+v", f.Published, want)
	}
	r := byName(Diagnose(f))
	if got := r["port 3478/udp"]; got.Verdict != Warn || !strings.Contains(got.Detail, "headscale's server") || !strings.HasPrefix(got.Fix, "ufw allow 3478/udp") {
		t.Fatalf("3478/udp without a rule: %+v", got)
	}
	if got := r["port 3479/udp"]; got.Verdict != Pass {
		t.Fatalf("3479/udp with a rule: %+v", got)
	}
	if _, listed := r["port 19090/tcp"]; listed {
		t.Fatal("a port on the loopback address is not reachable from outside")
	}
	if r["firewall"].Verdict != Pass {
		t.Fatalf("the firewall itself: %+v", r["firewall"])
	}
}

func TestAPortRuleWithoutAProtocolAllowsBoth(t *testing.T) {
	var f Facts
	f.Firewall.Allowed = []string{"3478", "80/tcp"}
	for port, want := range map[string]bool{"3478/udp": true, "3478/tcp": true, "80/tcp": true, "80/udp": false, "443/tcp": false} {
		if f.Allows(port) != want {
			t.Errorf("%s: allowed %v, want %v", port, !want, want)
		}
	}
}
