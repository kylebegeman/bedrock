package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture starts with a hooked guard and observes reachability after every
// firewall command. Real netfilter commits restore input as one transaction.
func TestWebGuardReconcileNeverLeavesItsLiveChainOpen(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"drop", "hooks"} {
		value := "1"
		if name == "hooks" {
			value = "9"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shim := `#!/bin/sh
set -eu
case "$0" in
 *-restore)
  input=$(cat)
  case "$input" in *'-A BEDROCK-WEB -j DROP'*) ;; *) exit 1;; esac
  [ "$(printf '%s\n' "$input" | tail -n 1)" = COMMIT ] || { printf 'invalid restore framing\n' >&2; exit 1; }
  counts=$(printf '%s\n' "$input" | awk '/^-D DOCKER-USER / {deleted++} /^-I DOCKER-USER / {inserted++} END {print deleted+0, inserted+0}')
  set -- $counts
  hooks=$(cat "$GUARD_FIXTURE/hooks")
  hooks=$((hooks-$1+$2))
  [ "$hooks" = 3 ] || { printf 'duplicate guard hooks remained: %s\n' "$hooks" >&2; exit 1; }
  printf 1 > "$GUARD_FIXTURE/drop"
  printf '%s' "$hooks" > "$GUARD_FIXTURE/hooks"
  ;;
 *)
  case "${1:-}" in
   -N) exit 1 ;;
   -S)
    if [ "${2:-}" = DOCKER-USER ]; then
     hooks=$(cat "$GUARD_FIXTURE/hooks")
     for spec in '-p tcp -m conntrack --ctorigdstport 80 --ctdir ORIGINAL -j BEDROCK-WEB' '-p tcp -m conntrack --ctorigdstport 443 --ctdir ORIGINAL -j BEDROCK-WEB' '-p udp -m conntrack --ctorigdstport 443 --ctdir ORIGINAL -j BEDROCK-WEB'; do
      copies=$((hooks/3))
      while [ "$copies" -gt 0 ]; do
       printf '%s\n' "-A DOCKER-USER $spec"
       copies=$((copies-1))
      done
     done
    fi
    exit 0 ;;

   -C) exit 0 ;;
   -F) printf 0 > "$GUARD_FIXTURE/drop" ;;
   -A) case "$*" in *'-j DROP') printf 1 > "$GUARD_FIXTURE/drop";; esac ;;
   -D) hooks=$(cat "$GUARD_FIXTURE/hooks"); [ "$hooks" -gt 0 ] || exit 1; printf '%s' "$((hooks-1))" > "$GUARD_FIXTURE/hooks" ;;
   -I) hooks=$(cat "$GUARD_FIXTURE/hooks"); printf '%s' "$((hooks+1))" > "$GUARD_FIXTURE/hooks" ;;
  esac
  ;;
esac
if [ "$(cat "$GUARD_FIXTURE/drop")" = 0 ] || [ "$(cat "$GUARD_FIXTURE/hooks")" = 0 ]; then
 printf 'public bypass after %s\n' "$*" >> "$GUARD_FIXTURE/exposure"
fi
`
	for _, name := range []string{"iptables", "ip6tables", "iptables-restore", "ip6tables-restore"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(shim), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(webGuardScript(testRanges.All(), edgePorts))
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GUARD_FIXTURE="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guard script: %v %s", err, out)
	}
	if exposure, err := os.ReadFile(filepath.Join(dir, "exposure")); err == nil {
		t.Fatalf("reconcile exposed published ports: %s", exposure)
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
