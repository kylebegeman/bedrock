package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/kernel"
)

func TestRemoteMoveHandoffIsReplayedAndBoundToApproval(t *testing.T) {
	config, record, captured := targetArtifactFixture(t)
	source := targetSource(t)
	handoff := filepath.Join(t.TempDir(), "handoff.json")
	payload := []byte("{\"fixture\":\"final capture\"}\n")
	if err := os.WriteFile(handoff, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"move", "stage", source, "--handoff", handoff}
	out, stderr, code := targetRun(t, config, append(args, "--plan", "--json")...)
	var plan kernel.PlanView
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	_, stderr, code = targetRun(t, config, append(args, "--digest", plan.Digest)...)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	in, _ := os.ReadFile(filepath.Join(captured, "applied-stdin"))
	argv, _ := os.ReadFile(record)
	if string(in) != string(payload) || !strings.Contains(string(argv), "--handoff=-\n") || strings.Contains(string(argv), handoff) {
		t.Fatalf("handoff transport: %s %s", in, argv)
	}
	os.Remove(record)
	os.WriteFile(handoff, []byte("{\"fixture\":\"changed\"}\n"), 0o600)
	_, stderr, code = targetRun(t, config, append(args, "--digest", plan.Digest)...)
	if code == 0 || !strings.Contains(stderr, "source snapshot") {
		t.Fatalf("changed handoff accepted: %d %s", code, stderr)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("changed handoff applied")
	}
}

func TestRemoteRecoveryPlanDoesNotReadIdentityAndApplyStreamsIt(t *testing.T) {
	config, record, captured := targetArtifactFixture(t)
	key := filepath.Join(t.TempDir(), "recovery.key")
	args := []string{"recover", "old-host", "--snapshot", strings.Repeat("a", 64), "--identity-file", key}
	out, stderr, code := targetRun(t, config, append(args, "--plan", "--json")...)
	var plan kernel.PlanView
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil {
		t.Fatalf("plan read nonexistent key: %d %s %s", code, out, stderr)
	}
	private := []byte("fixture source recovery identity\n")
	os.WriteFile(key, private, 0o600)
	_, stderr, code = targetRun(t, config, append(args, "--digest", plan.Digest)...)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	in, _ := os.ReadFile(filepath.Join(captured, "applied-stdin"))
	argv, _ := os.ReadFile(record)
	if string(in) != string(private) || !strings.Contains(string(argv), "--identity-stdin\n") || strings.Contains(string(argv), key) || strings.Contains(stderr, string(private)) {
		t.Fatal("identity was not streamed privately")
	}
	_, stderr, code = targetRun(t, config, "recover", "old-host", "--snapshot", strings.Repeat("a", 64), "--identity-stdin")
	if code == 0 || !strings.Contains(stderr, "stdin is reserved") {
		t.Fatalf("unapproved stdin recovery accepted: %d %s", code, stderr)
	}
}

func TestRemoteRecoveryPlanDoesNotReadLocalKey(t *testing.T) {
	config, _ := targetFixture(t)
	out, stderr, code := targetRun(t, config, "recover", "old-host", "--snapshot", "12345678", "--identity-file", "/missing/local/key", "--plan", "--json")
	if code != 0 || !strings.Contains(out, "remote-plan") {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}
