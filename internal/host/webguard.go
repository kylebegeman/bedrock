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
		// The chain is filled before anything jumps to it, so the edge is
		// never closed to Cloudflare on the way.
		w("%s%s -N %s 2>/dev/null || %s -F %s", indent, fam.ipt, webGuardChain, fam.ipt, webGuardChain)
		for _, s := range fam.sources {
			w("%s%s -A %s -s %s -j RETURN", indent, fam.ipt, webGuardChain, s)
		}
		w("%s%s -A %s -j DROP", indent, fam.ipt, webGuardChain)
		// Docker adopts a DOCKER-USER that is already there, and jumps to it
		// from FORWARD when it starts.
		w("%s%s -N DOCKER-USER 2>/dev/null || true", indent, fam.ipt)
		for _, p := range ports {
			w("%swhile %s -D DOCKER-USER %s 2>/dev/null; do :; done", indent, fam.ipt, hookSpec(p))
			w("%s%s -I DOCKER-USER %s", indent, fam.ipt, hookSpec(p))
		}
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
	// Sources are the ranges the guard lets through, IPv4.
	Sources []string `json:"sources,omitempty"`
	// Drops is whether the guard drops what it does not let through.
	Drops bool `json:"drops"`
	// Hooks is how many of the edge's ports DOCKER-USER sends to it.
	Hooks int `json:"hooks"`
}

// InPlace reports whether the guard exists, drops the rest and is hooked
// in for every port the edge publishes.
func (g WebGuard) InPlace() bool { return g.Drops && g.Hooks >= len(edgePorts) }

// gatherWebGuard reads the guard out of iptables.
func gatherWebGuard(ctx context.Context, env Env) WebGuard {
	var g WebGuard
	chain, err := env.Run(ctx, "iptables", "-S", webGuardChain)
	if err != nil {
		return g
	}
	for _, line := range strings.Split(chain, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 6 && f[0] == "-A" && f[2] == "-s" && f[4] == "-j" && f[5] == "RETURN":
			g.Sources = append(g.Sources, f[3])
		case len(f) == 4 && f[0] == "-A" && f[2] == "-j" && f[3] == "DROP":
			g.Drops = true
		}
	}
	if hooks, err := env.Run(ctx, "iptables", "-S", "DOCKER-USER"); err == nil {
		for _, p := range edgePorts {
			if strings.Contains(hooks, fmt.Sprintf("-p %s -m conntrack --ctorigdstport %d --ctdir ORIGINAL -j %s", p.Proto, p.Port, webGuardChain)) {
				g.Hooks++
			}
		}
	}
	return g
}

// guardCurrent reports whether the guard lets exactly the private
// networks and Cloudflare's IPv4 ranges through.
func guardCurrent(g WebGuard, ranges []string) bool {
	if !g.InPlace() {
		return false
	}
	want := slices.Clone(privateV4)
	for _, r := range ranges {
		if !strings.Contains(r, ":") {
			want = append(want, r)
		}
	}
	have := slices.Clone(g.Sources)
	slices.Sort(want)
	slices.Sort(have)
	return slices.Equal(want, have)
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
	if changed {
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
	if !env.Exists(webGuardScriptPath) && !env.Exists(webGuardUnitPath) {
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
	for _, p := range []string{webGuardUnitPath, webGuardScriptPath} {
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
