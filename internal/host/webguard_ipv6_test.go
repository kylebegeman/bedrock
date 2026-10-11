package host

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestFirewallReconcileChecksAvailableIPv6Guard(t *testing.T) {
	guardFor := func(sources []string) string {
		var rules strings.Builder
		rules.WriteString("-N BEDROCK-WEB\n")
		for _, source := range sources {
			rules.WriteString("-A BEDROCK-WEB -s " + source + " -j RETURN\n")
		}
		rules.WriteString("-A BEDROCK-WEB -j DROP\n")
		return rules.String()
	}
	v4 := guardFor(append(slices.Clone(privateV4), testRanges.IPv4...))
	v6 := guardFor(append(slices.Clone(privateV6), testRanges.IPv6...))
	var hooks strings.Builder
	for _, port := range edgePorts {
		hooks.WriteString("-A DOCKER-USER " + hookSpec(port) + "\n")
	}
	for _, tc := range []struct {
		name, guard, hooks string
		current            bool
	}{
		{name: "complete", guard: v6, hooks: hooks.String(), current: true},
		{name: "missing chain", hooks: hooks.String()},
		{name: "missing hooks", guard: v6},
		{name: "missing drop", guard: strings.ReplaceAll(v6, "-A BEDROCK-WEB -j DROP\n", ""), hooks: hooks.String()},
		{name: "stale sources", guard: strings.ReplaceAll(v6, testRanges.IPv6[0], "2001:db8::/32"), hooks: hooks.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setUpBox(t)
			m.answers["ufw status"] = "Status: active\nOpenSSH ALLOW Anywhere\n" +
				"80,443/tcp ALLOW 173.245.48.0/20 # bedrock: cloudflare\n" +
				"80,443/tcp ALLOW 103.21.244.0/22 # bedrock: cloudflare\n" +
				"80,443/tcp ALLOW 2400:cb00::/32 # bedrock: cloudflare\n"
			m.answers["iptables -S BEDROCK-WEB"] = v4
			m.answers["iptables -S DOCKER-USER"] = hooks.String()
			m.answers["ip6tables -S"] = ""
			if tc.guard != "" {
				m.answers["ip6tables -S BEDROCK-WEB"] = tc.guard
			}
			if tc.hooks != "" {
				m.answers["ip6tables -S DOCKER-USER"] = tc.hooks
			}
			f := Gather(context.Background(), m.env(), "")
			if current := firewallDone(f, testRanges.All()); current != tc.current {
				t.Fatalf("IPv6 %s reported current=%v, want %v", tc.name, current, tc.current)
			}
		})
	}
}

func TestWebGuardControlsDockerStartupOnlyWhileInstalled(t *testing.T) {
	const dependencyPath = "/etc/systemd/system/docker.service.d/bedrock-web-guard.conf"
	m := setUpBox(t)
	allowEverything(m)
	if err := ensureWebGuard(context.Background(), m.env(), io.Discard, testRanges.All()); err != nil {
		t.Fatal(err)
	}
	dependency := m.read(dependencyPath)
	if !strings.Contains(dependency, "Requires="+webGuardUnitName) || !strings.Contains(dependency, "After="+webGuardUnitName) {
		t.Fatalf("Docker must wait for a successful guard at startup: %q", dependency)
	}
	if strings.Contains(webGuardUnit, "After=docker.service") || strings.Contains(webGuardUnit, "Requires=docker.service") {
		t.Fatal("the guard cannot depend on the Docker service it precedes")
	}
	if err := removeWebGuard(context.Background(), m.env(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if m.env().Exists(dependencyPath) || m.env().Exists(webGuardUnitPath) || m.env().Exists(webGuardScriptPath) {
		t.Fatal("opening the web left Docker dependent on the guard")
	}
}

func TestRemovingAnOrphanedGuardDependencyReloadsDockerUnits(t *testing.T) {
	const dependencyPath = "/etc/systemd/system/docker.service.d/bedrock-web-guard.conf"
	m := setUpBox(t)
	allowEverything(m)
	m.write(dependencyPath, "[Unit]\nRequires="+webGuardUnitName+"\nAfter="+webGuardUnitName+"\n")
	if err := removeWebGuard(context.Background(), m.env(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if m.env().Exists(dependencyPath) || !m.ranCommand("systemctl daemon-reload") {
		t.Fatal("orphaned dependency remained active after guard removal")
	}
}

func TestWebGuardInspectionRejectsUnsafeRuleOrderAndExtras(t *testing.T) {
	for _, ipt := range []string{"iptables", "ip6tables"} {
		ipv6 := ipt == "ip6tables"
		sources := append(slices.Clone(privateV4), testRanges.IPv4...)
		if ipv6 {
			sources = append(slices.Clone(privateV6), testRanges.IPv6...)
		}
		var sourceRules strings.Builder
		for _, source := range sources {
			sourceRules.WriteString("-A BEDROCK-WEB -s " + source + " -j RETURN\n")
		}
		body := sourceRules.String()
		const drop = "-A BEDROCK-WEB -j DROP\n"
		var hooks strings.Builder
		for _, port := range edgePorts {
			hooks.WriteString("-A DOCKER-USER " + hookSpec(port) + "\n")
		}
		for _, tc := range []struct {
			name, rules    string
			current        bool
			duplicateHooks bool
		}{
			{name: "complete", rules: body + drop, current: true},
			{name: "duplicate hooks", rules: body + drop, duplicateHooks: true},
			{name: "unconditional return", rules: "-A BEDROCK-WEB -j RETURN\n" + body + drop},
			{name: "unconditional accept", rules: "-A BEDROCK-WEB -j ACCEPT\n" + body + drop},
			{name: "early drop", rules: drop + body},
			{name: "duplicate drop", rules: body + drop + drop},
			{name: "trailing return", rules: body + drop + "-A BEDROCK-WEB -j RETURN\n"},
			{name: "source order changed", rules: strings.Replace(body, "-s "+sources[0], "-s REORDERED", 1)},
		} {
			t.Run(ipt+"/"+tc.name, func(t *testing.T) {
				rules := tc.rules
				if tc.name == "source order changed" {
					rules = strings.Replace(rules, "-s "+sources[1], "-s "+sources[0], 1)
					rules = strings.Replace(rules, "-s REORDERED", "-s "+sources[1], 1) + drop
				}
				m := newFakeMachine(t)
				m.answers[ipt+" -S BEDROCK-WEB"] = "-N BEDROCK-WEB\n" + rules
				m.answers[ipt+" -S DOCKER-USER"] = hooks.String()
				if tc.duplicateHooks {
					m.answers[ipt+" -S DOCKER-USER"] += "-A DOCKER-USER " + hookSpec(edgePorts[0]) + "\n"
				}
				g := gatherWebGuardFamily(context.Background(), m.env(), ipt)
				if current := guardFamilyCurrent(g, testRanges.All(), ipv6); current != tc.current {
					t.Fatalf("unsafe chain reported current=%v, want %v: %s", current, tc.current, rules)
				}
			})
		}
	}
}
