package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kylebegeman/bedrock/internal/cloudflare"
)

// Where a machine takes web traffic from: the profile's web_from.
const (
	// WebFromAnyone opens 80 and 443 to every address. It is the default,
	// and what a route with dns: direct or a manual record needs.
	WebFromAnyone = "anyone"
	// WebFromCloudflare opens 80 and 443 only to Cloudflare's proxy, so the
	// machine's address answers nobody else on the web even once it is
	// known. Every route must then be dns: proxied.
	WebFromCloudflare = "cloudflare"
)

// cloudflareComment marks the rules bedrock keeps for Cloudflare's ranges,
// so it can tell them from rules a person added.
const cloudflareComment = "bedrock: cloudflare"

// webPorts is the edge's ports as one ufw rule allows them.
const webPorts = "80,443"

// RangesPath keeps the last list of Cloudflare's ranges bedrock applied, so
// a reconcile still works while the list cannot be fetched.
const RangesPath = "/etc/bedrock/cloudflare-ranges.json"

// ufwDefaults is ufw's own settings file; IPV6=no there means ufw leaves
// IPv6 alone entirely.
const ufwDefaults = "/etc/default/ufw"

// FirewallRule is one rule as `ufw status` shows it.
type FirewallRule struct {
	// Num is the rule's number in `ufw status numbered`; zero otherwise.
	Num int `json:"num,omitempty"`
	// To is the port, port list or application profile, such as 22/tcp,
	// 80,443/tcp or OpenSSH.
	To string `json:"to"`
	// Action is ALLOW, LIMIT, DENY or REJECT.
	Action string `json:"action"`
	// From is a source range, or "Anywhere".
	From string `json:"from"`
	// V6 is an IPv6 rule.
	V6      bool   `json:"v6,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Lets reports whether the rule lets traffic in (LIMIT does, at a rate).
func (r FirewallRule) Lets() bool { return r.Action == "ALLOW" || r.Action == "LIMIT" }

// Open reports whether the rule lets anyone in.
func (r FirewallRule) Open() bool { return r.Lets() && r.From == "Anywhere" }

// Web reports whether the rule is for the edge's ports.
func (r FirewallRule) Web() bool {
	switch r.To {
	case "80", "443", "80/tcp", "443/tcp", webPorts, webPorts + "/tcp", "443,80", "443,80/tcp":
		return true
	}
	return false
}

// ours reports whether bedrock keeps the rule for Cloudflare.
func (r FirewallRule) ours() bool { return strings.TrimSpace(r.Comment) == cloudflareComment }

// ufwRule reads one row of `ufw status` or `ufw status numbered`. An
// application profile's name can hold spaces ("Nginx Full"), so the row
// is split on its action rather than on whitespace.
var ufwRule = regexp.MustCompile(`^(?:\[\s*(\d+)\]\s*)?(.+?)\s+(ALLOW|LIMIT|DENY|REJECT)(?:\s+(?:IN|OUT|FWD))?\s+(.+?)\s*(?:#\s*(.*))?$`)

// parseUFW reads the rules out of `ufw status`, numbered or not.
func parseUFW(text string) []FirewallRule {
	var rules []FirewallRule
	for _, line := range strings.Split(text, "\n") {
		g := ufwRule.FindStringSubmatch(strings.TrimSpace(line))
		if g == nil {
			continue
		}
		r := FirewallRule{To: g[2], Action: g[3], From: g[4], Comment: strings.TrimSpace(g[5])}
		r.Num, _ = strconv.Atoi(g[1])
		if to, ok := strings.CutSuffix(r.To, " (v6)"); ok {
			r.To, r.V6 = to, true
		}
		if from, ok := strings.CutSuffix(r.From, " (v6)"); ok {
			r.From, r.V6 = from, true
		}
		if strings.Contains(r.From, ":") {
			r.V6 = true
		}
		rules = append(rules, r)
	}
	return rules
}

// openPorts is what the rules let anyone reach over IPv4, as the facts
// have always listed them.
func openPorts(rules []FirewallRule) []string {
	var open []string
	for _, r := range rules {
		if r.Open() && !r.V6 && !slices.Contains(open, r.To) {
			open = append(open, r.To)
		}
	}
	return open
}

// WebOpen reports whether the firewall lets anyone reach 80 or 443.
func (f Facts) WebOpen() bool {
	for _, r := range f.Firewall.Rules {
		if r.Open() && r.Web() {
			return true
		}
	}
	return false
}

// CloudflareSources are the ranges bedrock's Cloudflare rules let reach
// 80 and 443.
func (f Facts) CloudflareSources() []string {
	var out []string
	for _, r := range f.Firewall.Rules {
		if r.ours() && r.Lets() && !slices.Contains(out, r.From) {
			out = append(out, r.From)
		}
	}
	return out
}

// CloudflareOnly reports whether the machine's profile takes the web only
// from Cloudflare.
func (f Facts) CloudflareOnly() bool { return f.WebFrom == WebFromCloudflare }

// Route is one host an app on the machine routes, as the setup that
// closes the web to everyone but Cloudflare needs to see it.
type Route struct {
	App  string
	Host string
	// Mode is the route's dns: setting; "" is a manual record.
	Mode    string
	Proxied bool
}

// ErrRoutesReachDirectly is what setup says when the web cannot be closed
// to everyone but Cloudflare because some routes do not go through it.
var ErrRoutesReachDirectly = errors.New("routes reach this machine directly")

// requireProxied refuses routes that do not go through Cloudflare's proxy:
// with the web closed to everyone else, nothing could reach them, and a
// manual record is checked to point straight here, so it cannot either.
func requireProxied(routes []Route) error {
	var direct []string
	for _, r := range routes {
		if r.Proxied {
			continue
		}
		mode := r.Mode
		if mode == "" {
			mode = "manual"
		}
		direct = append(direct, fmt.Sprintf("%s (%s, dns: %s)", r.Host, r.App, mode))
	}
	if len(direct) == 0 {
		return nil
	}
	sort.Strings(direct)
	return fmt.Errorf("%w: taking the web only from Cloudflare needs every route behind its proxy, and %s would stop answering. Give them dns: proxied and deploy, then set this up again",
		ErrRoutesReachDirectly, strings.Join(direct, ", "))
}

// cloudflareRanges is the list of ranges one setup applies, and whether it
// came from Cloudflare just now or from the copy kept on the machine.
type cloudflareRanges struct {
	cloudflare.Ranges
	fresh bool
}

// fetchRanges reads Cloudflare's current ranges, falling back to the copy
// the last setup kept when they cannot be read.
func (s Setup) fetchRanges(ctx context.Context) (cloudflareRanges, error) {
	fetch := s.CloudflareRanges
	if fetch == nil {
		fetch = cloudflare.New("").IPs
	}
	got, err := fetch(ctx)
	if err == nil {
		if err = checkRanges(got); err == nil {
			return cloudflareRanges{Ranges: got, fresh: true}, nil
		}
	}
	kept, keptErr := loadRanges(s.Env)
	if keptErr != nil {
		return cloudflareRanges{}, fmt.Errorf("read Cloudflare's address ranges: %w; and no earlier copy on this machine: %v", err, keptErr)
	}
	return cloudflareRanges{Ranges: kept}, nil
}

// checkRanges refuses a list that would open the web to far more than
// Cloudflare: something other than addresses, or a range as wide as a
// continent. Cloudflare's are /12 to /22 and /29 to /32.
func checkRanges(r cloudflare.Ranges) error {
	if len(r.IPv4) == 0 {
		return errors.New("the list has no IPv4 ranges")
	}
	for _, list := range []struct {
		ranges []string
		v4     bool
		widest int
	}{{r.IPv4, true, 8}, {r.IPv6, false, 16}} {
		for _, s := range list.ranges {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return fmt.Errorf("%q is not an address range", s)
			}
			if p.Addr().Is4() != list.v4 || p != p.Masked() || p.Bits() < list.widest {
				return fmt.Errorf("%q is not a range bedrock will open the web to", s)
			}
		}
	}
	return nil
}

func loadRanges(env Env) (cloudflare.Ranges, error) {
	text, err := env.ReadFile(RangesPath)
	if err != nil {
		return cloudflare.Ranges{}, err
	}
	var r cloudflare.Ranges
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return cloudflare.Ranges{}, fmt.Errorf("%s: %w", RangesPath, err)
	}
	if err := checkRanges(r); err != nil {
		return cloudflare.Ranges{}, fmt.Errorf("%s: %w", RangesPath, err)
	}
	return r, nil
}

func saveRanges(env Env, r cloudflare.Ranges) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	_, err = env.WriteFile(RangesPath, string(b)+"\n", 0o644)
	return err
}

// ufwIPv6 reports whether ufw filters IPv6 on this machine.
func ufwIPv6(env Env) bool {
	text, err := env.ReadFile(ufwDefaults)
	if err != nil {
		return true // ufw's default
	}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == "IPV6" {
			return strings.Trim(v, `"' `) != "no"
		}
	}
	return true
}

// firewallDone says whether the firewall already is what the profile asks,
// judged from the rules the plan read.
func firewallDone(f Facts, ranges []string) bool {
	if !f.Firewall.Active || !f.Allows("22/tcp") {
		return false
	}
	guard := f.Firewall.WebGuard
	if ranges == nil {
		return f.Allows("80/tcp") && f.Allows("443/tcp") && len(f.CloudflareSources()) == 0 && !guard.Drops && guard.Hooks == 0
	}
	have := f.CloudflareSources()
	if f.WebOpen() || len(have) != len(ranges) || !guardCurrent(guard, ranges) {
		return false
	}
	for _, r := range ranges {
		if !slices.Contains(have, r) {
			return false
		}
	}
	return true
}

// ensureFirewall allows ssh before anything is denied, so a mistake can't
// lock the door. With ranges it lets only those reach 80 and 443: they are
// allowed before the open rules go, so the web never closes to Cloudflare
// on the way. Without, 80 and 443 are open to anyone and bedrock's
// Cloudflare rules, if an earlier setup made them, are removed.
func ensureFirewall(ctx context.Context, env Env, out io.Writer, ranges []string) error {
	ufw := func(args ...string) error {
		_, err := env.Run(ctx, "ufw", args...)
		return err
	}
	for _, args := range [][]string{{"allow", "OpenSSH"}, {"allow", "22/tcp"}} {
		if err := ufw(args...); err != nil {
			return err
		}
	}
	ipv6 := ufwIPv6(env)
	if ranges == nil {
		for _, port := range []string{"80/tcp", "443/tcp"} {
			if err := ufw("allow", port); err != nil {
				return err
			}
		}
	} else {
		skipped := 0
		for _, r := range ranges {
			if strings.Contains(r, ":") && !ipv6 {
				skipped++
				continue
			}
			// ufw skips a rule it already has, so this is safe to repeat.
			if err := ufw("allow", "proto", "tcp", "from", r, "to", "any", "port", webPorts, "comment", cloudflareComment); err != nil {
				return err
			}
		}
		if skipped > 0 {
			fmt.Fprintf(out, "ufw leaves IPv6 alone here (IPV6=no in %s), so %d IPv6 range(s) were not added and IPv6 is not filtered\n", ufwDefaults, skipped)
		}
	}
	for _, args := range [][]string{{"default", "deny", "incoming"}, {"default", "allow", "outgoing"}, {"--force", "enable"}} {
		if err := ufw(args...); err != nil {
			return err
		}
	}
	// ufw lists its rules only while it is active, so what goes is read
	// after enabling it.
	status, err := env.Run(ctx, "ufw", "status", "numbered")
	if err != nil {
		return err
	}
	var drop []FirewallRule
	for _, r := range parseUFW(status) {
		stale := r.ours() && (ranges == nil || !slices.Contains(ranges, r.From))
		if stale || (ranges != nil && r.Open() && r.Web()) {
			drop = append(drop, r)
		}
	}
	// Numbers shift down as rules go, so the last goes first.
	sort.Slice(drop, func(i, j int) bool { return drop[i].Num > drop[j].Num })
	for _, r := range drop {
		if r.Num == 0 {
			continue
		}
		if err := ufw("--force", "delete", strconv.Itoa(r.Num)); err != nil {
			return err
		}
	}
	if ranges == nil {
		fmt.Fprintf(out, "firewall active: ssh, 80, 443%s\n", removedNote(len(drop)))
		return nil
	}
	fmt.Fprintf(out, "firewall active: ssh; 80 and 443 only from Cloudflare's %d range(s)%s\n", len(ranges), removedNote(len(drop)))
	return nil
}

func removedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("; %d rule(s) removed", n)
}
