package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/api"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/ui"
)

// operate is what every mutating command does: show the plan, stop there
// with --plan, ask unless --yes (or refuse without a terminal), then run it
// and render the events.
func (a *app) operate(ctx context.Context, kind string, input any, planOnly bool) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	runner, err := a.runner(ctx, fmt.Sprintf("cli-%d", os.Getpid()))
	if err != nil {
		return err
	}
	defer runner.Close()
	r := a.renderer()
	view, err := runner.Plan(ctx, kind, raw)
	if err != nil {
		return err
	}
	if planOnly {
		r.Plan(view)
		return nil
	}
	if !a.yes {
		if !a.tty {
			r.Plan(view)
			return errors.New("this would change the machine; add --yes to apply it without a prompt, or --plan to only look")
		}
		r.Plan(view)
		fmt.Fprint(a.stdout, "Apply? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if s := strings.ToLower(strings.TrimSpace(answer)); s != "y" && s != "yes" {
			return errors.New("not applied")
		}
	}
	var lastID string
	receipt, err := runner.Run(ctx, kind, raw, func(ev kernel.Event) {
		if ev.Operation != "" {
			lastID = ev.Operation
		}
		r.Event(ev)
	})
	if errors.Is(err, api.ErrDisconnected) {
		if lastID != "" {
			return fmt.Errorf("%w; the daemon finishes or undoes %s on its own: bedrock history %s", err, lastID, lastID)
		}
		return err
	}
	if err != nil {
		return err
	}
	if receipt.Status != "succeeded" {
		return fmt.Errorf("%s: %s", receipt.Status, receipt.Error)
	}
	return nil
}

// mutatingFlags adds --plan and --yes to a command that changes the machine.
func (a *app) mutatingFlags(cmd *cobra.Command, planOnly *bool) {
	cmd.Flags().BoolVar(planOnly, "plan", false, "show the plan and change nothing")
	cmd.Flags().BoolVar(&a.yes, "yes", false, "apply without asking")
}

func newHistory(a *app) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history [operation-id]",
		Short: "List recent operations, or show one receipt.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			runner, err := a.runner(ctx, fmt.Sprintf("cli-%d", os.Getpid()))
			if err != nil {
				return err
			}
			defer runner.Close()
			r := a.renderer()
			if len(args) == 1 {
				receipt, err := runner.Receipt(ctx, args[0])
				if err != nil {
					return err
				}
				r.Receipt(receipt)
				return nil
			}
			rows, err := runner.List(ctx, limit)
			if err != nil {
				return err
			}
			summaries := make([]ui.Summary, 0, len(rows))
			for _, row := range rows {
				summaries = append(summaries, ui.Summary{ID: row.ID, Kind: row.Kind, Target: row.Target, Status: row.Status, CreatedAt: row.CreatedAt, Interruptions: row.Interruptions, Error: row.Error})
			}
			r.History(summaries)
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many operations to list")
	return cmd
}

func newKernel(a *app) *cobra.Command {
	kernelCmd := &cobra.Command{
		Use:   "kernel",
		Short: "Exercise the kernel itself.",
	}
	var (
		in       kernel.ExerciseInput
		planOnly bool
		recovery string
	)
	exercise := &cobra.Command{
		Use:   "exercise",
		Short: "Run a fixture operation that writes marker files, to prove journaling and recovery.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in.Recovery = kernel.Recovery(recovery)
			return a.operate(cmd.Context(), kernel.ExerciseKind, in, planOnly)
		},
	}
	exercise.Flags().StringVar(&in.Dir, "dir", "", "where the marker files go (required)")
	exercise.Flags().IntVar(&in.Steps, "steps", 4, "how many steps")
	exercise.Flags().StringVar(&in.StepDuration, "step-duration", "0s", "how long each step takes")
	exercise.Flags().IntVar(&in.FailAt, "fail-at", 0, "make this step fail (1-based)")
	exercise.Flags().StringVar(&recovery, "recovery", "resume", "resume or compensate after an interruption")
	a.mutatingFlags(exercise, &planOnly)
	_ = exercise.MarkFlagRequired("dir")
	kernelCmd.AddCommand(exercise)
	return kernelCmd
}
