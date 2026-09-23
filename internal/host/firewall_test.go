package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/cloudflare"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

// Two of Cloudflare's published ranges stand for the list.
var testRanges = cloudflare.Ranges{IPv4: []string{"173.245.48.0/20", "103.21.244.0/22"}, IPv6: []string{"2400:cb00::/32"}}

func rangesFrom(r cloudflare.Ranges, err error) func(context.Context) (cloudflare.Ranges, error) {
	return func(context.Context) (cloudflare.Ranges, error) { return r, err }
}

func routesOf(routes ...Route) func(context.Context) ([]Route, error) {
	return func(context.Context) ([]Route, error) { return routes, nil }
}

func cloudflareRegistry(m *fakeMachine, s Setup) kernel.Registry {
	s.Env = m.env()
	reg := kernel.Registry{}
	reg.Add(s)
	return reg
}

func cloudflareProfile() Profile {
	return Profile{Hostname: "personal-vps", SwapGiB: 4, WebFrom: WebFromCloudflare}
}

const statusOpen = `Status: active

     To                         Action      From
     --                         ------      ----
[ 1] OpenSSH                    ALLOW IN    Anywhere
[ 2] 80/tcp                     ALLOW IN    Anywhere
[ 3] 443/tcp                    ALLOW IN    Anywhere
[ 4] 80,443/tcp                 ALLOW IN    173.245.48.0/20            # bedrock: cloudflare
[ 5] 80,443/tcp                 ALLOW IN    103.21.244.0/22            # bedrock: cloudflare
[ 6] 80,443/tcp                 ALLOW IN    198.51.100.0/24            # bedrock: cloudflare
[ 7] OpenSSH (v6)               ALLOW IN    Anywhere (v6)
[ 8] 80/tcp (v6)                ALLOW IN    Anywhere (v6)
[ 9] 443/tcp (v6)               ALLOW IN    Anywhere (v6)
[10] 80,443/tcp                 ALLOW IN    2400:cb00::/32             # bedrock: cloudflare
`

func TestUFWRulesAreReadWithTheirSources(t *testing.T) {
	rules := parseUFW(statusOpen + "Nginx Full                 ALLOW       Anywhere\n22/tcp                     LIMIT       Anywhere\n5432/tcp                   DENY        10.0.0.0/8\n")
	if len(rules) != 13 {
		t.Fatalf("got %d rules: %+v", len(rules), rules)
	}
	for i, want := range []FirewallRule{
		{Num: 1, To: "OpenSSH", Action: "ALLOW", From: "Anywhere"},
		{Num: 4, To: "80,443/tcp", Action: "ALLOW", From: "173.245.48.0/20", Comment: "bedrock: cloudflare"},
		{Num: 8, To: "80/tcp", Action: "ALLOW", From: "Anywhere", V6: true},
		{Num: 10, To: "80,443/tcp", Action: "ALLOW", From: "2400:cb00::/32", V6: true, Comment: "bedrock: cloudflare"},
		{To: "Nginx Full", Action: "ALLOW", From: "Anywhere"},
		{To: "22/tcp", Action: "LIMIT", From: "Anywhere"},
		{To: "5432/tcp", Action: "DENY", From: "10.0.0.0/8"},
	} {
		idx := []int{0, 3, 7, 9, 10, 11, 12}[i]
		if rules[idx] != want {
			t.Errorf("rule %d: got %+v, want %+v", idx, rules[idx], want)
		}
	}
	f := Facts{Firewall: FirewallFacts{Rules: rules, Allowed: openPorts(rules)}}
	// A port open only to some sources is not open; a rate-limited one is.
	if f.Allows("5432/tcp") || !f.Allows("22/tcp") || !f.WebOpen() {
		t.Fatalf("allowed %v", f.Firewall.Allowed)
	}
	if got := f.CloudflareSources(); !slices.Equal(got, []string{"173.245.48.0/20", "103.21.244.0/22", "198.51.100.0/24", "2400:cb00::/32"}) {
		t.Fatalf("cloudflare sources %v", got)
	}
}

// The point of taking the web only from Cloudflare is that the machine's
// address stops answering; a route that does not go through Cloudflare
// would stop answering with it, so setup will not start.
func TestSetupWillNotCloseTheWebOnRoutesThatReachTheMachineDirectly(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	fetched := false
	s := Setup{
		Routes: routesOf(
			Route{App: "site", Host: "example.com", Mode: "proxied", Proxied: true},
			Route{App: "shop", Host: "shop.example.com", Mode: "direct"},
			Route{App: "blog", Host: "blog.example.org"},
		),
		CloudflareRanges: func(context.Context) (cloudflare.Ranges, error) { fetched = true; return testRanges, nil },
	}
	engine := kernel.New(openStore(t), cloudflareRegistry(m, s), "test")
	_, err := engine.PlanOnly(context.Background(), SetupKind, profileJSON(t, cloudflareProfile()))
	if !errors.Is(err, ErrRoutesReachDirectly) {
		t.Fatalf("want the routes refused, got %v", err)
	}
	for _, want := range []string{"blog.example.org (blog, dns: manual)", "shop.example.com (shop, dns: direct)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "example.com (site") || fetched {
		t.Fatalf("a proxied route was named, or the ranges were read for nothing: %v", err)
	}
}

func TestSetupLetsOnlyCloudflareReachTheWebWithoutEverClosingIt(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	m.answers["ufw status"] = statusOpen
	m.answers["ufw status numbered"] = statusOpen
	s := Setup{Routes: routesOf(Route{App: "site", Host: "example.com", Mode: "proxied", Proxied: true}), CloudflareRanges: rangesFrom(testRanges, nil)}
	engine := kernel.New(openStore(t), cloudflareRegistry(m, s), "test")
	view, err := engine.PlanOnly(context.Background(), SetupKind, profileJSON(t, cloudflareProfile()))
	if err != nil {
		t.Fatal(err)
	}
	var step kernel.PlanStepView
	for _, st := range view.Steps {
		if st.Name == "firewall" {
			step = st
		}
	}
	if step.Change != "allow ssh in, and 80 and 443 only from Cloudflare, Docker's published ports included" || step.Note != "the web is open to anyone" {
		t.Fatalf("firewall step: %+v", step)
	}
	receipt, err := engine.Run(context.Background(), SetupKind, profileJSON(t, cloudflareProfile()), func(kernel.Event) {})
	if err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("%v %+v", err, receipt)
	}
	cmds := m.commands()
	at := func(cmd string) int {
		i := slices.Index(cmds, cmd)
		if i < 0 {
			t.Fatalf("never ran %q in %v", cmd, cmds)
		}
		return i
	}
	allow := at("ufw allow proto tcp from 173.245.48.0/20 to any port 80,443 comment bedrock: cloudflare")
	at("ufw allow proto tcp from 2400:cb00::/32 to any port 80,443 comment bedrock: cloudflare")
	// SSH first; Cloudflare in before the open rules go; the open rules
	// and the range Cloudflare dropped go last-first, so numbers hold.
	if at("ufw allow OpenSSH") > allow || allow > at("ufw --force enable") {
		t.Fatalf("order: %v", cmds)
	}
	var deleted []string
	for _, c := range cmds {
		if n, ok := strings.CutPrefix(c, "ufw --force delete "); ok {
			deleted = append(deleted, n)
		}
	}
	if !slices.Equal(deleted, []string{"9", "8", "6", "3", "2"}) {
		t.Fatalf("deleted %v", deleted)
	}
	if at("ufw status numbered") < at("ufw --force enable") {
		t.Fatal("the rules must be read once ufw lists them, after it is enabled")
	}
	if kept, err := loadRanges(m.env()); err != nil || !slices.Equal(kept.All(), testRanges.All()) {
		t.Fatalf("kept ranges %v %v", kept, err)
	}
	if p, err := LoadProfile(m.env()); err != nil || !p.CloudflareOnly() {
		t.Fatalf("profile %+v %v", p, err)
	}
	// Docker forwards the edge's ports past ufw, so the guard goes in too,
	// now and at every boot, after ufw's rules.
	script := m.read(webGuardScriptPath)
	for _, want := range []string{"-s 173.245.48.0/20 -j RETURN", "-s 2400:cb00::/32 -j RETURN", "--ctorigdstport 443", "-A BEDROCK-WEB -j DROP"} {
		if !strings.Contains(script, want) {
			t.Errorf("the guard lacks %q", want)
		}
	}
	if !strings.Contains(m.read(webGuardUnitPath), "Before=docker.service") {
		t.Fatalf("unit: %s", m.read(webGuardUnitPath))
	}
	if at("ufw --force enable") > at("systemctl enable "+webGuardUnitName) || at("systemctl enable "+webGuardUnitName) > at("sh "+webGuardScriptPath) {
		t.Fatalf("guard order: %v", cmds)
	}
}

// The guard is where the web is really closed, so a machine whose ufw
// rules are right but whose guard is missing or stale is not done.
func TestSetupIsNotDoneUntilDockersPortsAreGuarded(t *testing.T) {
	onlyCloudflare := "Status: active\n\nOpenSSH ALLOW Anywhere\n" +
		"80,443/tcp ALLOW 173.245.48.0/20 # bedrock: cloudflare\n" +
		"80,443/tcp ALLOW 103.21.244.0/22 # bedrock: cloudflare\n" +
		"80,443/tcp ALLOW 2400:cb00::/32 # bedrock: cloudflare\n"
	guard := "-N BEDROCK-WEB\n"
	for _, s := range append(append([]string{}, privateV4...), testRanges.IPv4...) {
		guard += "-A BEDROCK-WEB -s " + s + " -j RETURN\n"
	}
	guard += "-A BEDROCK-WEB -j DROP\n"
	hooks := "-N DOCKER-USER\n"
	for _, p := range edgePorts {
		hooks += "-A DOCKER-USER " + hookSpec(p) + "\n"
	}
	for _, c := range []struct {
		name, guard, hooks, note string
	}{
		{"no guard", "", "", "Docker's published web ports are open to anyone"},
		{"not hooked in", guard, "-N DOCKER-USER\n", "Docker's published web ports are open to anyone"},
		{"stale ranges", strings.Replace(guard, "173.245.48.0/20", "198.51.100.0/24", 1), hooks, "Cloudflare's ranges changed"},
		{"in place", guard, hooks, "already only from Cloudflare"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := setUpBox(t)
			m.answers["ufw status"] = onlyCloudflare
			if c.guard != "" {
				m.answers["iptables -S BEDROCK-WEB"] = c.guard
				m.answers["iptables -S DOCKER-USER"] = c.hooks
			}
			_, notes := planStepsWith(t, m, Setup{CloudflareRanges: rangesFrom(testRanges, nil)}, cloudflareProfile())
			if notes["firewall"] != c.note {
				t.Fatalf("note %q, want %q", notes["firewall"], c.note)
			}
		})
	}
}

func TestSetupKeepsWorkingOnTheLastRangesWhenCloudflareCannotBeAsked(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	down := rangesFrom(cloudflare.Ranges{}, errors.New("no route to host"))
	engine := kernel.New(openStore(t), cloudflareRegistry(m, Setup{CloudflareRanges: down}), "test")
	if _, err := engine.PlanOnly(context.Background(), SetupKind, profileJSON(t, cloudflareProfile())); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Fatalf("with nothing kept, setup must stop: %v", err)
	}
	if err := saveRanges(m.env(), testRanges); err != nil {
		t.Fatal(err)
	}
	_, notes := planStepsWith(t, m, Setup{CloudflareRanges: down}, cloudflareProfile())
	if notes["cloudflare-ranges"] != "Cloudflare's list could not be read, so the copy kept is used" {
		t.Fatalf("note %q", notes["cloudflare-ranges"])
	}
}

// A list that would open the web to much more than Cloudflare is refused,
// whoever serves it.
func TestSetupRefusesRangesWiderThanCloudflares(t *testing.T) {
	for _, bad := range []cloudflare.Ranges{
		{},
		{IPv4: []string{"0.0.0.0/0"}},
		{IPv4: []string{"173.245.48.0/20", "10.0.0.0/7"}},
		{IPv4: []string{"173.245.48.1/20"}},
		{IPv4: []string{"2400:cb00::/32"}},
		{IPv4: []string{"173.245.48.0/20"}, IPv6: []string{"::/0"}},
		{IPv4: []string{"not a range"}},
	} {
		if err := bad.Check(); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if err := testRanges.Check(); err != nil {
		t.Fatal(err)
	}
}

// Taking the web from anyone again removes the rules bedrock made for
// Cloudflare, so a machine does not keep a list nobody refreshes.
func TestSetupOpensTheWebAgainAndForgetsCloudflaresRules(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	m.answers["ufw status"] = statusOpen
	m.answers["ufw status numbered"] = statusOpen
	_, notes := planSteps(t, m, Profile{Hostname: "personal-vps", SwapGiB: 4})
	if notes["firewall"] != "the web is open only to Cloudflare" {
		t.Fatalf("note %q", notes["firewall"])
	}
	engine := kernel.New(openStore(t), registryWith(m), "test")
	if receipt, err := engine.Run(context.Background(), SetupKind, profileJSON(t, Profile{Hostname: "personal-vps", SwapGiB: 4}), func(kernel.Event) {}); err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("%v %+v", err, receipt)
	}
	var deleted []string
	for _, c := range m.commands() {
		if n, ok := strings.CutPrefix(c, "ufw --force delete "); ok {
			deleted = append(deleted, n)
		}
	}
	if !slices.Equal(deleted, []string{"10", "6", "5", "4"}) || !m.ranCommand("ufw allow 80/tcp") {
		t.Fatalf("deleted %v in %v", deleted, m.commands())
	}
}

func TestOpeningTheWebAgainTakesTheGuardAway(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	m.write(webGuardScriptPath, webGuardScript(testRanges.All(), edgePorts))
	m.write(webGuardUnitPath, webGuardUnit)
	m.answers["iptables -S BEDROCK-WEB"] = "-N BEDROCK-WEB\n-A BEDROCK-WEB -j DROP\n"
	_, notes := planSteps(t, m, Profile{Hostname: "personal-vps", SwapGiB: 4})
	if notes["firewall"] != "the web is open only to Cloudflare" {
		t.Fatalf("note %q", notes["firewall"])
	}
	engine := kernel.New(openStore(t), registryWith(m), "test")
	if receipt, err := engine.Run(context.Background(), SetupKind, profileJSON(t, Profile{Hostname: "personal-vps", SwapGiB: 4}), func(kernel.Event) {}); err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("%v %+v", err, receipt)
	}
	cmds := m.commands()
	stop, disable := slices.Index(cmds, "sh "+webGuardScriptPath+" stop"), slices.Index(cmds, "systemctl disable "+webGuardUnitName)
	if stop < 0 || disable < stop || m.read(webGuardScriptPath) != "" || m.read(webGuardUnitPath) != "" {
		t.Fatalf("the guard stayed: %v", cmds)
	}
}

func TestSetupSaysWhenUFWLeavesIPv6Alone(t *testing.T) {
	m := setUpBox(t)
	allowEverything(m)
	m.write(ufwDefaults, "IPV6=no\n")
	var out strings.Builder
	if err := ensureFirewall(context.Background(), m.env(), &out, testRanges.All()); err != nil {
		t.Fatal(err)
	}
	if m.ranCommand("ufw allow proto tcp from 2400:cb00::/32") || !strings.Contains(out.String(), "IPv6 is not filtered") {
		t.Fatalf("%q %v", out.String(), m.commands())
	}
}

func TestTheDoctorHoldsTheFirewallToTheProfile(t *testing.T) {
	onlyCloudflare := strings.Join([]string{
		"Status: active", "",
		"OpenSSH                    ALLOW       Anywhere",
		"80,443/tcp                 ALLOW       173.245.48.0/20            # bedrock: cloudflare",
		"80,443/tcp                 ALLOW       2400:cb00::/32             # bedrock: cloudflare",
	}, "\n")
	for _, c := range []struct {
		name, status, ufwDefaults string
		verdict                   Verdict
		detail                    string
	}{
		{"closed to all but cloudflare", onlyCloudflare, "IPV6=yes", Pass, "active: ssh; 80 and 443 only from Cloudflare (2 ranges)"},
		{"docker's ports unguarded", onlyCloudflare + "\nunguarded", "IPV6=yes", Fail, "Docker forwards 80 and 443 past ufw, and bedrock's guard for them is not in place, so the edge answers anyone"},
		{"open to anyone", statusOpen, "IPV6=yes", Fail, "the web is open to anyone, though this machine takes it only from Cloudflare"},
		{"closed to cloudflare too", "Status: active\n\nOpenSSH ALLOW Anywhere\n", "IPV6=yes", Fail, "80 and 443 are closed to Cloudflare too"},
		{"ipv6 unfiltered", onlyCloudflare, "IPV6=no", Warn, "ufw leaves IPv6 alone (IPV6=no), so the web ports are not filtered there"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := setUpBox(t)
			m.answers["ufw status"] = strings.TrimSuffix(c.status, "\nunguarded")
			if !strings.HasSuffix(c.status, "\nunguarded") {
				m.answers["iptables -S BEDROCK-WEB"] = "-N BEDROCK-WEB\n-A BEDROCK-WEB -s 173.245.48.0/20 -j RETURN\n-A BEDROCK-WEB -j DROP"
				hooks := ""
				for _, p := range edgePorts {
					hooks += "-A DOCKER-USER " + hookSpec(p) + "\n"
				}
				m.answers["iptables -S DOCKER-USER"] = hooks
			}
			m.write(ufwDefaults, c.ufwDefaults+"\n")
			if err := SaveProfile(m.env(), cloudflareProfile()); err != nil {
				t.Fatal(err)
			}
			got := byName(Diagnose(Gather(context.Background(), m.env(), "")))["firewall"]
			if got.Verdict != c.verdict || got.Detail != c.detail {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestAProfileTakesTheWebFromAnyoneOrCloudflare(t *testing.T) {
	for _, from := range []string{"", WebFromAnyone, WebFromCloudflare} {
		if err := (Profile{Hostname: "box", WebFrom: from}).Validate(); err != nil {
			t.Errorf("%q: %v", from, err)
		}
	}
	if err := (Profile{Hostname: "box", WebFrom: "fastly"}).Validate(); err == nil {
		t.Fatal("an unknown source must be refused")
	}
}

func planStepsWith(t *testing.T, m *fakeMachine, s Setup, p Profile) (*kernel.PlanView, map[string]string) {
	t.Helper()
	engine := kernel.New(openStore(t), cloudflareRegistry(m, s), "test")
	view, err := engine.PlanOnly(context.Background(), SetupKind, profileJSON(t, p))
	if err != nil {
		t.Fatal(err)
	}
	notes := map[string]string{}
	for _, st := range view.Steps {
		notes[st.Name] = st.Note
	}
	return view, notes
}

// Every machine keeps Cloudflare's list, so the edge can name the visitor
// behind a proxied route. A machine open to anyone carries on when the
// list cannot be read.
func TestEveryMachineKeepsCloudflaresRangesWithoutDependingOnThem(t *testing.T) {
	m := freshUbuntu(t)
	allowEverything(m)
	engine := kernel.New(openStore(t), registryWith(m), "test")
	receipt, err := engine.Run(context.Background(), SetupKind, profileJSON(t, Profile{Hostname: "personal-vps"}), func(kernel.Event) {})
	if err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("%v %+v", err, receipt)
	}
	if kept, err := loadRanges(m.env()); err != nil || !slices.Equal(kept.All(), testRanges.All()) {
		t.Fatalf("kept %v %v", kept, err)
	}

	m = freshUbuntu(t)
	allowEverything(m)
	down := Setup{CloudflareRanges: rangesFrom(cloudflare.Ranges{}, errors.New("no route to host"))}
	engine = kernel.New(openStore(t), cloudflareRegistry(m, down), "test")
	receipt, err = engine.Run(context.Background(), SetupKind, profileJSON(t, Profile{Hostname: "personal-vps"}), func(kernel.Event) {})
	if err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("a list that cannot be read must not stop setup: %v %+v", err, receipt)
	}
	if m.read(RangesPath) != "" {
		t.Fatal("nothing should be kept")
	}
}
