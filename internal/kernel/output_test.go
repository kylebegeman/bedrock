package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/state"
)

type diagnosticOutput struct{}

func (diagnosticOutput) Kind() string { return "test.output" }
func (diagnosticOutput) Plan(context.Context, json.RawMessage) (*Plan, error) {
	return &Plan{Target: "api", Recovery: Resume, Steps: []Step{
		{Name: "build-web", Apply: func(_ context.Context, out io.Writer) error {
			for i := 0; i < 45; i++ {
				fmt.Fprintf(out, "build line %d\n", i)
			}
			return nil
		}},
		{Name: "verify", Apply: func(_ context.Context, out io.Writer) error {
			fmt.Fprintln(out, "verification detail")
			return errors.New("verification failed")
		}},
		{Name: "retire", Apply: func(context.Context, io.Writer) error { return nil }},
	}}, nil
}

func TestSavedOutputKeepsBoundedTailsAndDoesNotChangeReceipts(t *testing.T) {
	store := newStore(t)
	reg := Registry{}
	reg.Add(diagnosticOutput{})
	e := New(store, reg, "test")
	ctx := context.Background()
	receipt, err := e.Run(ctx, "test.output", json.RawMessage(`{}`), func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != state.Failed {
		t.Fatalf("expected failed verification: %+v", receipt)
	}
	raw, _ := json.Marshal(receipt)
	if strings.Contains(string(raw), `"output"`) || strings.Contains(string(raw), "verification detail") {
		t.Fatal("regular receipts must not include saved output")
	}
	saved, err := e.OutputOf(ctx, receipt.ID, "")
	if err != nil || len(saved.Steps) != 3 || saved.TailLinesPerExecution != 40 {
		t.Fatalf("saved output: %v %+v", err, saved)
	}
	lines := strings.Split(strings.TrimSuffix(saved.Steps[0].Output, "\n"), "\n")
	if len(lines) != 40 || lines[0] != "build line 5" || lines[39] != "build line 44" {
		t.Fatalf("tail: %v", lines)
	}
	if saved.Steps[1].Output != "verification detail\n" || saved.Steps[1].Status != state.StepFailed || saved.Steps[2].Output != "" || saved.Steps[2].Status != state.StepPending {
		t.Fatalf("step output states: %+v", saved.Steps)
	}
	for _, selector := range []string{"verify", "2"} {
		one, err := e.OutputOf(ctx, receipt.ID, selector)
		if err != nil || len(one.Steps) != 1 || one.Steps[0].Name != "verify" {
			t.Fatalf("selected %q: %v %+v", selector, err, one)
		}
	}
	if _, err := e.OutputOf(ctx, receipt.ID, "missing"); err == nil || !strings.Contains(err.Error(), "build-web") {
		t.Fatalf("bad step selector: %v", err)
	}
	if _, err := e.OutputOf(ctx, "missing", ""); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("missing operation: %v", err)
	}
}

func TestDiagnosticOutputBoundsUnbrokenLines(t *testing.T) {
	var emitted []string
	w := newLineWriter(func(line string) { emitted = append(emitted, line) })
	if _, err := w.Write([]byte(strings.Repeat("x", 5<<20))); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if len(emitted) != 1 || len(emitted[0]) > 65<<10 {
		t.Fatalf("unbounded diagnostic event: %d event(s), %d bytes", len(emitted), len(emitted[0]))
	}
	if len(w.Tail()) > 65<<10 || !strings.Contains(w.Tail(), "truncated") {
		t.Fatalf("unbounded or silently shortened saved diagnostic: %d bytes", len(w.Tail()))
	}
}
