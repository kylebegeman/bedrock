package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellArgumentsRemainData(t *testing.T) {
	args := []string{"printf", "%s", `a' $(printf injected); \"`}
	cmd, err := Transport{}.Command(context.Background(), Target{SSH: "root@example.com"}, Shell(args...))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.Args[len(cmd.Args)-1], `'"'"'`) {
		t.Fatalf("unquoted apostrophe: %v", cmd.Args)
	}
	if cmd.Args[len(cmd.Args)-2] != "root@example.com" {
		t.Fatal("destination was not its own argument")
	}
	for _, s := range []string{"-oProxyCommand=sh", "host;touch x", "host\nother", ""} {
		if (Target{SSH: s}).Validate() == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestPinnedConnectionsAndExplicitSudo(t *testing.T) {
	target := Target{SSH: "production", Port: 2222, IdentityFile: "/tmp/key with spaces", KnownHostsFile: "/tmp/known hosts", Sudo: true}
	cmd, err := Transport{}.Command(context.Background(), target, Shell(target.Arguments("run", "api", "--", "sh", "-c", "echo '$HOME'")...))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, "\n")
	for _, want := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes", "UserKnownHostsFile=/tmp/known hosts", "'sudo' '-n' '--' '/usr/local/bin/bedrock'"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
}

func TestTargetConfigurationIsPrivateAndVersioned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "targets.json")
	config := Config{Version: 1, Targets: map[string]Target{"business": {SSH: "root@business.example.com"}}}
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Targets["business"].SSH != config.Targets["business"].SSH {
		t.Fatalf("roundtrip: %+v %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions %v", info.Mode())
	}
	for _, bad := range []string{`{"version":2,"targets":{}}`, `{"version":1,"targets":{},"unknown":true}`, `{"version":1,"targets":{}} {}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestPlanDigestCannotBeAppliedToAnotherTarget(t *testing.T) {
	one := Target{SSH: "one"}
	two := Target{SSH: "two"}
	digest := ScopeDigest(one, "012345")
	if raw, err := UnscopeDigest(one, digest); err != nil || raw != "012345" {
		t.Fatalf("unwrap %s %v", raw, err)
	}
	if _, err := UnscopeDigest(two, digest); err == nil {
		t.Fatal("another target accepted the plan")
	}
	if _, err := UnscopeDigest(one, "012345"); err == nil {
		t.Fatal("local digest accepted as target-bound")
	}
}

func TestBindingIncludesRemoteStateAndSocket(t *testing.T) {
	original := Binding{Target: Target{SSH: "fixture", MachineID: "machine"}, StateArgs: []string{"--state-dir=/srv/one", "--socket=/run/one.sock"}}
	token := original.ScopeDigest("same-plan")
	if raw, err := original.UnscopeDigest(token); err != nil || raw != "same-plan" {
		t.Fatalf("original binding refused: %s %v", raw, err)
	}
	for _, args := range [][]string{{"--state-dir=/srv/two", "--socket=/run/one.sock"}, {"--state-dir=/srv/one", "--socket=/run/two.sock"}, nil} {
		changed := original
		changed.StateArgs = args
		if _, err := changed.UnscopeDigest(token); err == nil {
			t.Fatal("different remote state or socket accepted approval")
		}
	}
}
