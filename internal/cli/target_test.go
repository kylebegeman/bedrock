package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/remote"
)

func targetFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "arguments")
	binary := filepath.Join(dir, "bedrock")
	ssh := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
case "$*" in
 *capabilities*) printf '%s\n' '{"api_version":1,"manifest_version":1,"state_version":1,"machine_id":"fixture-machine","capabilities":["remote.source.v1","kernel.plan.v1","kernel.apply.v1","kernel.expect.v1"]}'; exit 0 ;;
 *--plan*) printf '%s\n' '{"kind":"kernel.exercise","target":"fixture","recovery":"resume","steps":[{"index":0,"name":"one","change":"fixture change"}],"digest":"remote-plan"}'; exit 0 ;;
esac
printf '%s\n' "$@" > "$BEDROCK_TEST_REMOTE_ARGS"
printf 'remote applied\n'
exit "${BEDROCK_TEST_REMOTE_EXIT:-0}"
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nfor last do :; done\nexec /bin/sh -c \"$last\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEDROCK_SSH", ssh)
	t.Setenv("BEDROCK_TEST_REMOTE_ARGS", record)
	config := filepath.Join(dir, "targets.json")
	if err := remote.Save(config, remote.Config{Version: 1, Targets: map[string]remote.Target{"one": {SSH: "fixture", Binary: binary}, "two": {SSH: "other", Binary: binary}}}); err != nil {
		t.Fatal(err)
	}
	return config, record
}
func targetRun(t *testing.T, config string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"--targets-file", config, "--target", "one"}, args...)
	code := Main(full, &out, &errOut)
	return out.String(), errOut.String(), code
}
func TestRemotePlansBindTargetAndMachine(t *testing.T) {
	config, record := targetFixture(t)
	out, stderr, code := targetRun(t, config, "kernel", "exercise", "--dir", "/tmp/markers", "--plan", "--json")
	var plan kernel.PlanView
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	if plan.Digest == "remote-plan" || !strings.Contains(plan.Digest, ".") {
		t.Fatal("unbound plan")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("plan applied")
	}
	_, stderr, code = targetRun(t, config, "kernel", "exercise", "--dir", "/tmp/markers", "--digest", plan.Digest)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	data, _ := os.ReadFile(record)
	if !strings.Contains(string(data), "--digest=remote-plan") {
		t.Fatalf("%s", data)
	}
	var stdout, errOut bytes.Buffer
	code = Main([]string{"--targets-file", config, "--target", "two", "kernel", "exercise", "--dir", "/tmp/markers", "--digest", plan.Digest}, &stdout, &errOut)
	if code == 0 || !strings.Contains(errOut.String(), "another target") {
		t.Fatalf("cross target accepted: %d %s", code, errOut.String())
	}
	c, _ := remote.Load(config)
	one := c.Targets["one"]
	one.MachineID = "changed-machine"
	c.Targets["one"] = one
	if err := remote.Save(config, c); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = targetRun(t, config, "version")
	if code == 0 || !strings.Contains(stderr, "identity changed") {
		t.Fatalf("%d %s", code, stderr)
	}
}
func TestRemoteHasNoLocalFallbackAndPreservesExit(t *testing.T) {
	config, record := targetFixture(t)
	t.Setenv("BEDROCK_TEST_REMOTE_EXIT", "42")
	_, _, code := targetRun(t, config, "version")
	if code != 42 {
		t.Fatalf("exit %d", code)
	}
	os.Remove(record)
	_, stderr, code := targetRun(t, config, "kernel", "exercise", "--dir", "/tmp/markers")
	if code == 0 || !strings.Contains(stderr, "add --yes") {
		t.Fatalf("%d %s", code, stderr)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("refusal applied")
	}
	t.Setenv("BEDROCK_SSH", filepath.Join(t.TempDir(), "missing"))
	_, stderr, code = targetRun(t, config, "version")
	if code == 0 || !strings.Contains(stderr, "capabilities unavailable") {
		t.Fatalf("%d %s", code, stderr)
	}
}
func TestRemoteCommandSeparatorAndRepeatedFlags(t *testing.T) {
	config, record := targetFixture(t)
	_, stderr, code := targetRun(t, config, "--json", "run", "api", "web", "--secret", "FIRST", "--secret", "SECOND", "--", "sh", "-c", `printf '%s' '$(touch bad)'`)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	data, _ := os.ReadFile(record)
	got := string(data)
	want := "run\n--secret=FIRST\n--secret=SECOND\napi\nweb\n--json\n--\nsh\n-c\nprintf '%s' '$(touch bad)'\n"
	if got != want {
		t.Fatalf("args %q want %q", got, want)
	}
}
func TestTargetConfigurationAndVerification(t *testing.T) {
	config, _ := targetFixture(t)
	var out, errOut bytes.Buffer
	if code := Main([]string{"--targets-file", config, "target", "verify", "one"}, &out, &errOut); code != 0 {
		t.Fatalf("%d %s", code, errOut.String())
	}
	c, _ := remote.Load(config)
	if c.Targets["one"].MachineID != "fixture-machine" {
		t.Fatal("identity not pinned")
	}
	if code := Main([]string{"--targets-file", config, "target", "set", "bad", "--ssh", "host;touch bad"}, &out, &errOut); code == 0 {
		t.Fatal("injected destination accepted")
	}
}
func TestSourceSnapshotChangesApproval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bedrock.yaml")
	if err := os.WriteFile(path, []byte("app: fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, hash, err := snapshotSource(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	defer os.Remove(a.Name())
	b, same, err := snapshotSource(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	defer os.Remove(b.Name())
	if hash != same {
		t.Fatal("identical source hashes differently")
	}
	os.WriteFile(path, []byte("app: changed\n"), 0o600)
	c, changed, err := snapshotSource(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	defer os.Remove(c.Name())
	if hash == changed {
		t.Fatal("source changes did not alter snapshot")
	}
	binding := remote.Binding{Target: remote.Target{SSH: "fixture", MachineID: "fixture-machine"}, SourceHash: hash}
	token := binding.ScopeDigest("plan")
	binding.SourceHash = changed
	if _, err := binding.UnscopeDigest(token); err == nil {
		t.Fatal("changed source accepted approval")
	}
}

func TestRemoteEnvironmentStateOverridesStayConsistent(t *testing.T) {
	config, record := targetFixture(t)
	t.Setenv("BEDROCK_STATE_DIR", "/target/custom-state")
	t.Setenv("BEDROCK_SOCKET", "/target/custom.sock")
	_, stderr, code := targetRun(t, config, "version")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	got, _ := os.ReadFile(record)
	for _, flag := range []string{"--state-dir=/target/custom-state", "--socket=/target/custom.sock"} {
		if !strings.Contains(string(got), flag) {
			t.Fatalf("final command lost environment state: %s", got)
		}
	}
}
