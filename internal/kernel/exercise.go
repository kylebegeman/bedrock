package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ExerciseKind is the kernel's own test fixture: an operation that writes
// one marker file per step, slowly, and can be told to fail. It exists so a
// machine can prove crash recovery without touching anything real.
const ExerciseKind = "kernel.exercise"

// ExerciseInput configures an exercise run.
type ExerciseInput struct {
	// Dir is where the marker files go.
	Dir string `json:"dir"`
	// Steps is how many steps to run.
	Steps int `json:"steps"`
	// StepDuration is how long each step takes, as a Go duration.
	StepDuration string `json:"step_duration,omitempty"`
	// FailAt makes that step (1-based) fail. Zero means none.
	FailAt int `json:"fail_at,omitempty"`
	// Recovery is resume (default) or compensate.
	Recovery Recovery `json:"recovery,omitempty"`
}

// Exercise is the Definition for ExerciseKind.
type Exercise struct{}

// Kind implements Definition.
func (Exercise) Kind() string { return ExerciseKind }

// Plan implements Definition.
func (Exercise) Plan(_ context.Context, raw json.RawMessage) (*Plan, error) {
	var in ExerciseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("exercise input: %w", err)
	}
	if in.Dir == "" {
		return nil, errors.New("exercise input: dir is required")
	}
	if in.Steps < 1 || in.Steps > 1000 {
		return nil, errors.New("exercise input: steps must be 1 to 1000")
	}
	var pause time.Duration
	if in.StepDuration != "" {
		d, err := time.ParseDuration(in.StepDuration)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("exercise input: step_duration %q is not a duration", in.StepDuration)
		}
		pause = d
	}
	recovery := in.Recovery
	switch recovery {
	case "", Resume:
		recovery = Resume
	case Compensate:
	default:
		return nil, fmt.Errorf("exercise input: recovery must be resume or compensate, not %q", recovery)
	}
	plan := &Plan{Target: in.Dir, Recovery: recovery}
	for n := 1; n <= in.Steps; n++ {
		path := filepath.Join(in.Dir, fmt.Sprintf("step-%d", n))
		failing := n == in.FailAt
		change := fmt.Sprintf("write %s", path)
		if failing {
			change += " (asked to fail)"
		}
		plan.Steps = append(plan.Steps, Step{
			Name:   fmt.Sprintf("step-%d", n),
			Change: change,
			Apply: func(ctx context.Context, out io.Writer) error {
				if err := sleep(ctx, pause); err != nil {
					return err
				}
				if failing {
					return fmt.Errorf("asked to fail at step %d", n)
				}
				if err := os.MkdirAll(in.Dir, 0o755); err != nil {
					return err
				}
				if _, err := os.Stat(path); err == nil {
					fmt.Fprintf(out, "%s already there\n", path)
					return nil
				}
				if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(out, "wrote %s\n", path)
				return nil
			},
			Undo: func(_ context.Context, out io.Writer) error {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				fmt.Fprintf(out, "removed %s\n", path)
				return nil
			},
		})
	}
	return plan, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
