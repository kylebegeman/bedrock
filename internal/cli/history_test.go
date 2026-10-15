package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/state"
	"github.com/kylebegeman/helios/internal/ui"
)

func TestHistoryFiltersBeforeApplyingTheLimit(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ctx := context.Background()
	for i, row := range []struct {
		id, target, kind string
		status           state.OperationStatus
	}{
		{"old-failure", "api", "app.deploy", state.Failed},
		{"first-failure", "api", "app.deploy", state.Failed},
		{"newest-failure", "api", "app.deploy", state.Compensated},
		{"successful", "api", "app.deploy", state.Succeeded},
		{"other-kind", "api", "app.backup", state.Failed},
		{"other-app", "site", "app.deploy", state.Failed},
	} {
		op := state.NewOperation{ID: row.id, Target: row.target, Kind: row.kind, Input: json.RawMessage(`{}`), Plan: json.RawMessage(`{}`), CreatedAt: start.Add(time.Duration(i) * time.Minute)}
		if err := store.CreateOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, row.id, row.status, "", nil, op.CreatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		ids  []string
	}{
		{"combined", []string{"--app", "api", "--kind", "app.deploy", "--failed", "--since", start.Add(time.Minute).Format(time.RFC3339), "--limit", "1"}, []string{"newest-failure"}},
		{"app", []string{"--app", "api", "--limit", "2"}, []string{"other-kind", "successful"}},
		{"kind", []string{"--kind", "app.backup"}, []string{"other-kind"}},
		{"failed", []string{"--failed", "--limit", "3"}, []string{"other-app", "other-kind", "newest-failure"}},
		{"relative", []string{"--since", "2h", "--limit", "1"}, []string{"other-app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--json", "history"}, tc.args...)
			out, errOut, code := run(t, stateDir, args...)
			var rows []ui.Summary
			if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != len(tc.ids) {
				t.Fatalf("history: code %d, stderr %q, output %q", code, errOut, out)
			}
			for i, id := range tc.ids {
				if rows[i].ID != id {
					t.Fatalf("row %d = %s, want %s", i, rows[i].ID, id)
				}
			}
		})
	}
}

func TestHistoryOutputIsExplicitAndSelectsAStep(t *testing.T) {
	stateDir, dir := t.TempDir(), t.TempDir()
	out, errOut, code := run(t, stateDir, "--json", "kernel", "exercise", "--dir", dir, "--steps", "2", "--yes")
	if code != 0 {
		t.Fatalf("exercise: %d %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var last kernel.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Receipt == nil {
		t.Fatalf("receipt: %v %q", err, out)
	}
	id := last.Receipt.ID
	out, errOut, code = run(t, stateDir, "--json", "history", id)
	if code != 0 || strings.Contains(out, `"output"`) || strings.Contains(out, "wrote ") {
		t.Fatalf("ordinary receipt must omit output: %d %q %q", code, out, errOut)
	}
	for _, step := range []string{"step-2", "2"} {
		out, errOut, code = run(t, stateDir, "history", id, "--output", "--step", step)
		if code != 0 || !strings.Contains(out, "saved output tails") || !strings.Contains(out, "not a complete log") || !strings.Contains(out, "wrote "+filepath.Join(dir, "step-2")) || strings.Contains(out, "wrote "+filepath.Join(dir, "step-1")) {
			t.Fatalf("selected saved output: %d %q %q", code, out, errOut)
		}
	}
	out, errOut, code = run(t, stateDir, "--json", "history", id, "--output", "--step", "step-1")
	var saved struct {
		ID        string `json:"id"`
		TailLines int    `json:"tail_lines_per_execution"`
		Steps     []struct {
			Name   string `json:"name"`
			Output string `json:"output"`
		} `json:"steps"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &saved) != nil || saved.ID != id || saved.TailLines != 40 || len(saved.Steps) != 1 || saved.Steps[0].Name != "step-1" || !strings.Contains(saved.Steps[0].Output, "wrote ") {
		t.Fatalf("JSON saved output: %d %q %q", code, out, errOut)
	}
	_, errOut, code = run(t, stateDir, "history", id, "--output", "--step", "missing")
	if code == 0 || !strings.Contains(errOut, "step-1") || !strings.Contains(errOut, "step-2") {
		t.Fatalf("unknown step needs actionable choices: %d %q", code, errOut)
	}
}

func TestHistoryRejectsInvalidAndConflictingOptions(t *testing.T) {
	for i, args := range [][]string{
		{"history", "--since", "yesterday"},
		{"history", "--since", "-1h"},
		{"history", "--limit", "0"},
		{"history", "--output"},
		{"history", "op-1", "--step", "record"},
		{"history", "op-1", "--app", "api"},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, errOut, code := run(t, t.TempDir(), args...)
			if code == 0 || errOut == "" {
				t.Fatalf("invalid options accepted: %v", args)
			}
		})
	}
}
