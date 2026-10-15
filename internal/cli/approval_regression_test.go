package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/api"
	"github.com/kylebegeman/helios/internal/kernel"
)

type approvalRunner struct {
	api.Runner
	expected string
}

func (r *approvalRunner) Plan(context.Context, string, json.RawMessage) (*kernel.PlanView, error) {
	return &kernel.PlanView{Kind: kernel.ExerciseKind, Target: "fixture", Digest: "reviewed-digest"}, nil
}
func (r *approvalRunner) RunExpecting(_ context.Context, _ string, _ json.RawMessage, expect string, _ func(kernel.Event)) (*kernel.Receipt, error) {
	r.expected = expect
	return &kernel.Receipt{Status: "succeeded"}, nil
}
func (r *approvalRunner) Close() error { return nil }

func TestInteractiveApprovalPinsTheDisplayedPlan(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writer.WriteString("yes\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	original := os.Stdin
	os.Stdin = reader
	defer func() { os.Stdin = original }()
	runner := &approvalRunner{}
	app := &app{stdout: io.Discard, stderr: io.Discard, tty: true}
	if err := app.operateRaw(context.Background(), runner, kernel.ExerciseKind, json.RawMessage(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if runner.expected != "reviewed-digest" {
		t.Fatalf("approved displayed plan without pinning it: %q", runner.expected)
	}
}

func TestExecAndRunRejectExtraWorkloadArguments(t *testing.T) {
	for _, command := range []string{"exec", "run"} {
		t.Run(command, func(t *testing.T) {
			_, stderr, code := run(t, t.TempDir(), command, "notes", "web", "unexpected", "--", "true")
			if code == 0 || !strings.Contains(stderr, "optionally the workload") {
				t.Fatalf("extra workload argument was not rejected: %d %s", code, stderr)
			}
		})
	}
}
