package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Docker publishes the edge's ports by forwarding them to its container,
// and forwarded packets never meet the input rules ufw keeps: a ufw rule
// cannot say who reaches a published port. Docker does send every
// forwarded packet through its DOCKER-USER chain first, so that is where
// bedrock says it. The guard is a chain of its own, jumped to from
// DOCKER-USER for the edge's ports, that lets Cloudflare's ranges and
// private networks through and drops everything else.
const (
	webGuardChain      = "BEDROCK-WEB"
	webGuardScriptPath = "/etc/bedrock/web-guard.sh"
	webGuardUnitName   = "bedrock-web-guard.service"
	webGuardUnitPath   = "/etc/systemd/system/" + webGuardUnitName
	webGuardDockerPath = "/etc/systemd/system/docker.service.d/bedrock-web-guard.conf"
)

// guardPort is one published port of the edge's.
type guardPort struct {
	Proto string
	Port  int
}

// edgePorts are what the edge publishes: HTTP, HTTPS and HTTP/3.
var edgePorts = []guardPort{{"tcp", 80}, {"tcp", 443}, {"udp", 443}}

// Private sources reach the edge from the machine itself, its containers
// and a private network, never from the internet, and keep reaching it.
var (
	privateV4 = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}
	privateV6 = []string{"::1/128", "fc00::/7", "fe80::/10"}
)

// webGuardUnit runs the guard at boot, once ufw has loaded and before
// Docker starts the edge, so the ports are never open to everyone.
const webGuardUnit = `# Written by bedrock host setup. Lets only Cloudflare reach the web edge.
[Unit]
Description=bedrock: only Cloudflare reaches the web edge
After=ufw.service
Before=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh ` + webGuardScriptPath + `

[Install]
WantedBy=multi-user.target
`

// Requiring the guard from Docker's own unit makes a failed restore prevent
// Docker startup. This drop-in exists only while the Cloudflare guard is kept.
const webGuardDockerDependency = `[Unit]
Requires=` + webGuardUnitName + `
After=` + webGuardUnitName + `
`

// webGuardScript writes the guard as a shell script: run, it puts the
// guard in place; run with "stop", it takes it away. Every step is safe to
// repeat. ranges are Cloudflare's, both families.
func webGuardScript(ranges []string, ports []guardPort) string {
	var v4, v6 []string
	for _, r := range ranges {
		if strings.Contains(r, ":") {
			v6 = append(v6, r)
		} else {
			v4 = append(v4, r)
		}
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("#!/bin/sh")
	w("# Written by bedrock host setup; bedrock owns it and rewrites it.")
	w("#")
	w("# Docker forwards the web edge's published ports to its container, past")
	w("# the input rules ufw keeps. Every forwarded packet goes through")
	w("# DOCKER-USER first, so the guard is hooked in there: only Cloudflare's")
	w("# proxy and private networks reach the edge. \"stop\" takes it away.")
	w("set -eu")
	w("")
	w(`if [ "${1:-start}" = stop ]; then`)
	for _, ipt := range []string{"iptables", "ip6tables"} {
		w("  if command -v %s >/dev/null 2>&1; then", ipt)
		for _, p := range ports {
			w("    while %s -D DOCKER-USER %s 2>/dev/null; do :; done", ipt, hookSpec(p))
		}
		w("    %s -F %s 2>/dev/null || true", ipt, webGuardChain)
		w("    %s -X %s 2>/dev/null || true", ipt, webGuardChain)
		w("  fi")
	}
	w("  exit 0")
	w("fi")
	for _, fam := range []struct {
		ipt     string
		sources []string
	}{{"iptables", append(slices.Clone(privateV4), v4...)}, {"ip6tables", append(slices.Clone(privateV6), v6...)}} {
		w("")
		indent := ""
		if fam.ipt == "ip6tables" {
			// A machine without IPv6 filtering has nothing to guard there.
			w("if command -v ip6tables >/dev/null 2>&1 && ip6tables -S >/dev/null 2>&1; then")
			indent = "  "
		}
		// Preserve the existing guard until one restore transaction commits the
		// replacement chain and hooks. --noflush retains all other filter rules;
		// only the declared BEDROCK-WEB chain is replaced.
		w("%s%s -N DOCKER-USER 2>/dev/null || true", indent, fam.ipt)
		w("%sbedrock_web_hooks=$(%s -S DOCKER-USER)", indent, fam.ipt)
		w("%s{", indent)
		w("%s  cat <<'BEDROCK_WEB_CHAIN'", indent)
		w("*filter")
		w(":%s - [0:0]", webGuardChain)
		for _, source := range fam.sources {
			w("-A %s -s %s -j RETURN", webGuardChain, source)
		}
		w("-A %s -j DROP", webGuardChain)
		w("BEDROCK_WEB_CHAIN")
		for _, port := range ports {
			spec := hookSpec(port)
			// Delete every matching old hook from the snapshot in the same
			// transaction, then insert one before other DOCKER-USER rules.
			w("%s  printf '%%s\\n' \"$bedrock_web_hooks\" | while IFS= read -r bedrock_web_rule; do", indent)
			w("%s    if [ \"$bedrock_web_rule\" = '-A DOCKER-USER %s' ]; then", indent, spec)
			w("%s      printf '%%s\\n' '-D DOCKER-USER %s'", indent, spec)
			w("%s    fi", indent)
			w("%s  done", indent)
			w("%s  printf '%%s\\n' '-I DOCKER-USER %s'", indent, spec)
		}
		w("%s  printf '%%s\\n' COMMIT", indent)
		w("%s} | %s-restore --wait --noflush", indent, fam.ipt)
		if indent != "" {
			w("fi")
		}
	}
	return b.String()
}

// hookSpec matches a packet on its way to the edge by the port it was sent
// to, before Docker rewrote its destination, in the direction a client
// sends: replies from the edge are never caught.
func hookSpec(p guardPort) string {
	return fmt.Sprintf("-p %s -m conntrack --ctorigdstport %d --ctdir ORIGINAL -j %s", p.Proto, p.Port, webGuardChain)
}

// WebGuard is what the facts say of the guard.
type WebGuard struct {
	// Sources are the allowed ranges in this address family, in rule order.
	Sources []string `json:"sources,omitempty"`
	// Drops is whether the guard drops what it does not let through.
	Drops bool `json:"drops"`
	// Malformed means the owned guard contains unexpected rules or duplicate hooks.
	Malformed bool `json:"malformed,omitempty"`
	// Hooks counts the jumps for the edge's published ports.
	Hooks int `json:"hooks"`
	// IPv6 is the corresponding guard when IPv6 filtering is available.
	IPv6 *WebGuard `json:"ipv6,omitempty"`
}

// InPlace reports whether the guard exists, drops the rest and is hooked
// in for every port the edge publishes.
func (g WebGuard) InPlace() bool {
	return !g.Malformed && g.Drops && g.Hooks == len(edgePorts) && (g.IPv6 == nil || g.IPv6.InPlace())
}

// Present reports whether either family still has guard rules or hooks.
func (g WebGuard) Present() bool {
	return g.Drops || g.Hooks > 0 || len(g.Sources) > 0 || (g.IPv6 != nil && g.IPv6.Present())
}

// gatherWebGuard reads both available netfilter families.
func gatherWebGuard(ctx context.Context, env Env) WebGuard {
	g := gatherWebGuardFamily(ctx, env, "iptables")
	if _, err := env.Run(ctx, "ip6tables", "-S"); err == nil {
		v6 := gatherWebGuardFamily(ctx, env, "ip6tables")
		g.IPv6 = &v6
	}
	return g
}

func gatherWebGuardFamily(ctx context.Context, env Env, ipt string) WebGuard {
	var g WebGuard
	chain, err := env.Run(ctx, ipt, "-S", webGuardChain)
	if err != nil {
		return g
	}
	for _, line := range strings.Split(chain, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case len(f) == 2 && f[0] == "-N" && f[1] == webGuardChain:
		case len(f) == 6 && f[0] == "-A" && f[1] == webGuardChain && f[2] == "-s" && f[4] == "-j" && f[5] == "RETURN":
			if g.Drops {
				g.Malformed = true
			}
			g.Sources = append(g.Sources, f[3])
		case len(f) == 4 && f[0] == "-A" && f[1] == webGuardChain && f[2] == "-j" && f[3] == "DROP":
			if g.Drops {
				g.Malformed = true
			}
			g.Drops = true
		default:
			// Bedrock owns this chain completely. An ignored ACCEPT or RETURN
			// could bypass the final DROP while all expected sources remain.
			g.Malformed = true
		}
	}
	if hooks, err := env.Run(ctx, ipt, "-S", "DOCKER-USER"); err == nil {
		for _, p := range edgePorts {
			matches := 0
			for _, line := range strings.Split(hooks, "\n") {
				if strings.TrimSpace(line) == "-A DOCKER-USER "+hookSpec(p) {
					matches++
				}
			}
			g.Hooks += matches
			if matches > 1 {
				g.Malformed = true
			}
		}
	}
	return g
}

// guardCurrent reports whether every available guard lets exactly the
// private networks and Cloudflare's corresponding ranges through.
func guardCurrent(g WebGuard, ranges []string) bool {
	return guardFamilyCurrent(g, ranges, false) && (g.IPv6 == nil || guardFamilyCurrent(*g.IPv6, ranges, true))
}

func guardFamilyCurrent(g WebGuard, ranges []string, ipv6 bool) bool {
	if !g.InPlace() {
		return false
	}
	want := slices.Clone(privateV4)
	if ipv6 {
		want = slices.Clone(privateV6)
	}
	for _, r := range ranges {
		if strings.Contains(r, ":") == ipv6 {
			want = append(want, r)
		}
	}
	return slices.Equal(want, g.Sources)
}

// ensureWebGuard puts the guard in place now and at every boot.
func ensureWebGuard(ctx context.Context, env Env, out io.Writer, ranges []string) error {
	if _, err := env.WriteFile(webGuardScriptPath, webGuardScript(ranges, edgePorts), 0o700); err != nil {
		return err
	}
	changed, err := env.WriteFile(webGuardUnitPath, webGuardUnit, 0o644)
	if err != nil {
		return err
	}
	dependencyChanged, err := env.WriteFile(webGuardDockerPath, webGuardDockerDependency, 0o644)
	if err != nil {
		return err
	}
	if changed || dependencyChanged {
		if _, err := env.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	if _, err := env.Run(ctx, "systemctl", "enable", webGuardUnitName); err != nil {
		return err
	}
	if _, err := env.Run(ctx, "sh", webGuardScriptPath); err != nil {
		return fmt.Errorf("put the web guard in place: %w", err)
	}
	fmt.Fprintln(out, "Docker's published web ports take only Cloudflare and private networks")
	return nil
}

// removeWebGuard takes the guard away, when the web is open to anyone.
func removeWebGuard(ctx context.Context, env Env, out io.Writer) error {
	if !env.Exists(webGuardScriptPath) && !env.Exists(webGuardUnitPath) && !env.Exists(webGuardDockerPath) {
		return nil
	}
	if env.Exists(webGuardScriptPath) {
		if _, err := env.Run(ctx, "sh", webGuardScriptPath, "stop"); err != nil {
			return fmt.Errorf("take the web guard away: %w", err)
		}
	}
	if env.Exists(webGuardUnitPath) {
		if _, err := env.Run(ctx, "systemctl", "disable", webGuardUnitName); err != nil {
			return err
		}
	}
	for _, p := range []string{webGuardUnitPath, webGuardScriptPath, webGuardDockerPath} {
		if err := os.Remove(env.Path(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := env.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	fmt.Fprintln(out, "the web guard is gone; Docker's published web ports are open to anyone")
	return nil
}
