package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/state"
)

func events() []kernel.Event {
	t0 := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	return []kernel.Event{
		{Type: kernel.EventPlanned, Operation: "op-1", At: t0, Plan: &kernel.PlanView{Kind: "kernel.exercise", Target: "/tmp/x", Steps: []kernel.PlanStepView{{Name: "step-1"}, {Name: "step-2"}}}},
		{Type: kernel.EventStepStarted, Operation: "op-1", Step: &kernel.StepView{Index: 0, Name: "step-1", Status: state.StepRunning}},
		{Type: kernel.EventStepOutput, Operation: "op-1", Step: &kernel.StepView{Index: 0, Name: "step-1"}, Line: "wrote /tmp/x/step-1"},
		{Type: kernel.EventStepFinished, Operation: "op-1", Step: &kernel.StepView{Index: 0, Name: "step-1", Status: state.StepSucceeded, Duration: 1200 * time.Millisecond}},
		{Type: kernel.EventStepStarted, Operation: "op-1", Step: &kernel.StepView{Index: 1, Name: "step-2", Status: state.StepRunning}},
		{Type: kernel.EventStepFinished, Operation: "op-1", Step: &kernel.StepView{Index: 1, Name: "step-2", Status: state.StepFailed, Error: "asked to fail", Duration: 300 * time.Millisecond}},
		{Type: kernel.EventFinished, Operation: "op-1", Receipt: &kernel.Receipt{ID: "op-1", Status: state.Failed, Error: "step-2: asked to fail", Duration: 1500 * time.Millisecond, Interruptions: 1}},
	}
}

func TestPlainOutputIsOneLinePerEvent(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, false, false)
	for _, ev := range events() {
		r.Event(ev)
	}
	want := strings.Join([]string{
		"kernel.exercise on /tmp/x (2 steps) op-1",
		"  ...  step-1",
		"       | wrote /tmp/x/step-1",
		"  ok   step-1 (1.2s)",
		"  ...  step-2",
		"  fail step-2 (300ms): asked to fail",
		"failed in 1.5s, interrupted once: step-2: asked to fail",
		"receipt op-1",
		"",
	}, "\n")
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestTerminalOutputRewritesTheRunningLine(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, true, false)
	for _, ev := range events() {
		r.Event(ev)
	}
	s := out.String()
	if !strings.Contains(s, "\r\033[K  ...  step-1: wrote /tmp/x/step-1") {
		t.Fatalf("running line not rewritten in place:\n%q", s)
	}
	// The plan line, two finished steps, the outcome and the receipt line.
	if strings.Count(s, "\n") != 5 {
		t.Fatalf("want 5 lines, got %d:\n%q", strings.Count(s, "\n"), s)
	}
	if !strings.HasSuffix(s, "receipt op-1\n") {
		t.Fatalf("unexpected ending:\n%q", s)
	}
}

func TestJSONOutputIsOneObjectPerEvent(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, true, true)
	for _, ev := range events() {
		r.Event(ev)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(events()) {
		t.Fatalf("want %d lines, got %d", len(events()), len(lines))
	}
	var back kernel.Event
	if err := json.Unmarshal([]byte(lines[0]), &back); err != nil || back.Type != kernel.EventPlanned || back.Plan == nil {
		t.Fatalf("first line: %v %+v", err, back)
	}
}

func TestPlanAndReceiptRender(t *testing.T) {
	var out bytes.Buffer
	r := New(&out, false, false)
	r.Plan(&kernel.PlanView{Kind: "kernel.exercise", Target: "/tmp/x", Recovery: kernel.Compensate, Digest: "abc", Steps: []kernel.PlanStepView{{Index: 0, Name: "step-1", Change: "write /tmp/x/step-1"}}})
	if got := out.String(); got != "kernel.exercise on /tmp/x: 1 steps, undo on interruption\n   1. write /tmp/x/step-1\nplan digest abc\n" {
		t.Fatalf("plan:\n%q", got)
	}
	out.Reset()
	r.Receipt(&kernel.Receipt{ID: "op-1", Kind: "kernel.exercise", Target: "/tmp/x", Status: state.Succeeded, Duration: 2 * time.Second, StartedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		Steps: []kernel.StepView{{Name: "step-1", Status: state.StepSucceeded, Duration: time.Second, Attempts: 2}}})
	if got := out.String(); !strings.Contains(got, "succeeded, 1 steps, 2.0s") || !strings.Contains(got, "  ok   step-1 (1.0s), 2 attempts") {
		t.Fatalf("receipt:\n%q", got)
	}
}

func TestDuration(t *testing.T) {
	cases := map[time.Duration]string{0: "0s", 250 * time.Millisecond: "250ms", 1500 * time.Millisecond: "1.5s", 90 * time.Second: "1m30s"}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}
