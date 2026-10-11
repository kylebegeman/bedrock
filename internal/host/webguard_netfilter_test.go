package host

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// This opt-in check changes only a fresh network namespace. It deliberately
// fails rather than skips when an opted-in machine cannot run real netfilter.
func TestWebGuardLinuxNetfilter(t *testing.T) {
	if os.Getenv("BEDROCK_NETFILTER_TESTS") != "1" {
		t.Skip("set BEDROCK_NETFILTER_TESTS=1 to verify isolated Linux netfilter")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("netfilter verification requires Linux and root")
	}
	for _, tool := range []string{"unshare", "sh", "iptables", "iptables-restore", "ip6tables", "ip6tables-restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("netfilter verification requires %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	var script strings.Builder
	script.WriteString(`set -eu
for ipt in iptables ip6tables; do
  "$ipt" -N BEDROCK-UNRELATED
  "$ipt" -A BEDROCK-UNRELATED -j RETURN
  "$ipt" -A INPUT -j BEDROCK-UNRELATED
  "$ipt" -N DOCKER-USER
  "$ipt" -A FORWARD -j DOCKER-USER
  "$ipt" -A DOCKER-USER -j RETURN
done
iptables -S > "$BEDROCK_NETFILTER_OUTPUT/before-v4"
ip6tables -S > "$BEDROCK_NETFILTER_OUTPUT/before-v6"
`)
	// Installing a guard and replacing its source list exercise both restore
	// paths. The second transaction must replace the chain without extra hooks.
	ranges := [][]string{
		{testRanges.IPv4[0], testRanges.IPv6[0]},
		testRanges.All(),
	}
	for i, sources := range ranges {
		if i == 1 {
			// Seed duplicate hooks to ensure reconciliation removes every old
			// occurrence in its transaction, leaving exactly one per port.
			for _, ipt := range []string{"iptables", "ip6tables"} {
				for _, port := range edgePorts {
					for range 2 {
						fmt.Fprintf(&script, "%s -A DOCKER-USER %s\n", ipt, hookSpec(port))
					}
				}
			}
		}
		script.WriteString(webGuardScript(sources, edgePorts))
		fmt.Fprintf(&script, "iptables -S > \"$BEDROCK_NETFILTER_OUTPUT/after-%d-v4\"\n", i)
		fmt.Fprintf(&script, "ip6tables -S > \"$BEDROCK_NETFILTER_OUTPUT/after-%d-v6\"\n", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "unshare", "--net", "sh", "-s")
	cmd.Env = append(os.Environ(), "BEDROCK_NETFILTER_OUTPUT="+dir)
	cmd.Stdin = strings.NewReader(script.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated netfilter transactions: %v\n%s", err, out)
	}
	for _, family := range []string{"v4", "v6"} {
		before := readNetfilterRules(t, filepath.Join(dir, "before-"+family))
		slices.Sort(before)
		for i, sources := range ranges {
			after := readNetfilterRules(t, filepath.Join(dir, fmt.Sprintf("after-%d-%s", i, family)))
			preserved := make([]string, 0, len(after))
			var guard, hooks []string
			for _, line := range after {
				switch {
				case line == "-N "+webGuardChain:
				case strings.HasPrefix(line, "-A "+webGuardChain+" "):
					guard = append(guard, line)
				case strings.HasPrefix(line, "-A DOCKER-USER ") && strings.HasSuffix(line, "-j "+webGuardChain):
					hooks = append(hooks, line)
				default:
					preserved = append(preserved, line)
				}
			}
			slices.Sort(preserved)
			if !slices.Equal(before, preserved) {
				t.Fatalf("transaction %d %s altered unrelated rules:\nbefore %q\nafter %q", i, family, before, preserved)
			}
			wantSources := slices.Clone(privateV4)
			if family == "v6" {
				wantSources = slices.Clone(privateV6)
			}
			for _, source := range sources {
				if strings.Contains(source, ":") == (family == "v6") {
					wantSources = append(wantSources, source)
				}
			}
			if len(guard) != len(wantSources)+1 {
				t.Fatalf("transaction %d %s guard rule count: %q", i, family, guard)
			}
			for j, source := range wantSources {
				fields := strings.Fields(guard[j])
				if len(fields) != 6 || fields[2] != "-s" || fields[4] != "-j" || fields[5] != "RETURN" || normalizedPrefix(t, fields[3]) != normalizedPrefix(t, source) {
					t.Fatalf("transaction %d %s source %s rule: %q", i, family, source, guard[j])
				}
			}
			if guard[len(guard)-1] != "-A "+webGuardChain+" -j DROP" {
				t.Fatalf("transaction %d %s must end in DROP: %q", i, family, guard)
			}
			if len(hooks) != len(edgePorts) {
				t.Fatalf("transaction %d %s hook count %d, want %d: %q", i, family, len(hooks), len(edgePorts), hooks)
			}
			seen := make(map[guardPort]bool)
			for _, hook := range hooks {
				fields := strings.Fields(hook)
				if len(fields) != 12 {
					t.Fatalf("unexpected %s hook: %q", family, hook)
				}
				options := make(map[string]string)
				for j := 2; j < len(fields); j += 2 {
					options[fields[j]] = fields[j+1]
				}
				matched := false
				for _, port := range edgePorts {
					if options["-p"] == port.Proto && options["--ctorigdstport"] == fmt.Sprint(port.Port) && options["-m"] == "conntrack" && options["--ctdir"] == "ORIGINAL" && options["-j"] == webGuardChain && !seen[port] {
						seen[port], matched = true, true
						break
					}
				}
				if !matched {
					t.Fatalf("unexpected or duplicate %s hook: %q", family, hook)
				}
			}
		}
	}
}

func readNetfilterRules(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func normalizedPrefix(t *testing.T, source string) netip.Prefix {
	t.Helper()
	if prefix, err := netip.ParsePrefix(source); err == nil {
		return prefix.Masked()
	}
	addr, err := netip.ParseAddr(source)
	if err != nil {
		t.Fatalf("invalid netfilter source %q: %v", source, err)
	}
	return netip.PrefixFrom(addr, addr.BitLen())
}
