package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/version"
)

// run executes quark with a private state directory and no daemon.
func run(t *testing.T, stateDir string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"--state-dir", stateDir, "--socket", filepath.Join(stateDir, "no.sock")}, args...)
	code := Main(full, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestVersionPrintsThisBuild(t *testing.T) {
	out, errOut, code := run(t, t.TempDir(), "version")
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut)
	}
	if want := version.Current().String() + "\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	out, _, _ = run(t, t.TempDir(), "--json", "version")
	var info version.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil || info != version.Current() {
		t.Fatalf("json version: %v %q", err, out)
	}
}

func TestUnknownCommandFailsOnce(t *testing.T) {
	_, errOut, code := run(t, t.TempDir(), "nope")
	if code != 1 || strings.Count(errOut, "unknown command") != 1 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestPlanLooksAndChangesNothing(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "markers")
	out, errOut, code := run(t, stateDir, "kernel", "exercise", "--dir", dir, "--steps", "2", "--plan")
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut)
	}
	if !strings.HasPrefix(out, "kernel.exercise on "+dir+": 2 steps, resume on interruption\n") || !strings.Contains(out, "plan digest ") {
		t.Fatalf("plan output:\n%s", out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("--plan wrote markers")
	}
	out, _, _ = run(t, stateDir, "history")
	if !strings.Contains(out, "no operations yet") {
		t.Fatalf("--plan recorded an operation:\n%s", out)
	}
}

func TestWithoutATerminalApplyingNeedsYes(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "markers")
	_, errOut, code := run(t, stateDir, "kernel", "exercise", "--dir", dir)
	if code != 1 || !strings.Contains(errOut, "add --yes") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("refusing must change nothing")
	}
}

func TestApplyRunsAndHistoryRemembers(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "markers")
	out, errOut, code := run(t, stateDir, "--json", "kernel", "exercise", "--dir", dir, "--steps", "2", "--yes")
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last kernel.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != kernel.EventFinished || last.Receipt == nil || last.Receipt.Status != "succeeded" {
		t.Fatalf("last event: %v %+v", err, last)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-2")); err != nil {
		t.Fatal("the operation didn't write its markers")
	}
	out, _, code = run(t, stateDir, "history")
	if code != 0 || !strings.Contains(out, last.Receipt.ID) || !strings.Contains(out, "succeeded") {
		t.Fatalf("history:\n%s", out)
	}
	out, _, code = run(t, stateDir, "history", last.Receipt.ID)
	if code != 0 || !strings.Contains(out, "  ok   write "+filepath.Join(dir, "step-1")) {
		t.Fatalf("receipt:\n%s", out)
	}
}

func TestAFailedOperationFailsTheCommand(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(t.TempDir(), "markers")
	out, errOut, code := run(t, stateDir, "kernel", "exercise", "--dir", dir, "--steps", "3", "--fail-at", "2", "--recovery", "compensate", "--yes")
	if code != 1 || !strings.Contains(errOut, "compensated: step-2: asked to fail at step 2") {
		t.Fatalf("code %d, stderr %q, stdout %q", code, errOut, out)
	}
	if !strings.Contains(out, "  undid step-1") {
		t.Fatalf("stdout:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "step-1")); err == nil {
		t.Fatal("step-1 should have been undone")
	}
}
