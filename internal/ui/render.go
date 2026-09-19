// Package ui renders kernel events for people: one updating line per step
// in a terminal, one line per event anywhere else, or raw JSON for scripts.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/state"
)

// Renderer writes events to out.
type Renderer struct {
	out   io.Writer
	tty   bool
	json  bool
	width int
	enc   *json.Encoder
	live  bool // a step line is on screen and can be rewritten
	names map[int]string
}

// New returns a renderer. In a terminal (tty) a running step is one line
// that updates in place; otherwise every event is its own line. With
// jsonMode each event is written as one JSON object per line.
func New(out io.Writer, tty, jsonMode bool) *Renderer {
	r := &Renderer{out: out, tty: tty, json: jsonMode, width: 100, names: map[int]string{}}
	if jsonMode {
		r.enc = json.NewEncoder(out)
	}
	return r
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Event renders one event.
func (r *Renderer) Event(ev kernel.Event) {
	if r.json {
		_ = r.enc.Encode(ev)
		return
	}
	switch ev.Type {
	case kernel.EventPlanned:
		if ev.Plan != nil {
			r.println(fmt.Sprintf("%s on %s (%d steps) %s", ev.Plan.Kind, ev.Plan.Target, len(ev.Plan.Steps), ev.Operation))
		}
	case kernel.EventResumed:
		r.println(fmt.Sprintf("resumed %s: %s", ev.Operation, ev.Message))
	case kernel.EventStepStarted:
		if ev.Step == nil {
			return
		}
		verb := "..."
		if ev.Step.Undoing {
			verb = "undo"
		}
		r.names[ev.Step.Index] = ev.Step.Name
		r.running(fmt.Sprintf("  %-4s %s", verb, ev.Step.Name))
	case kernel.EventStepOutput:
		if ev.Step == nil {
			return
		}
		if r.tty {
			r.running(fmt.Sprintf("  ...  %s: %s", ev.Step.Name, ev.Line))
		} else {
			r.println("       | " + ev.Line)
		}
	case kernel.EventStepFinished:
		if ev.Step == nil {
			return
		}
		r.finished(*ev.Step)
	case kernel.EventFinished:
		r.endLine()
		if ev.Receipt != nil {
			r.println(outcome(ev.Receipt))
		} else {
			r.println(ev.Message)
		}
	case kernel.EventError:
		r.endLine()
		r.println("error: " + ev.Message)
	}
}

func (r *Renderer) finished(st kernel.StepView) {
	word := "ok"
	switch {
	case st.Status == state.StepFailed && st.Undoing:
		word = "fail"
	case st.Status == state.StepFailed:
		word = "fail"
	case st.Status == state.StepUndone:
		word = "undid"
	}
	line := fmt.Sprintf("  %-4s %s (%s)", word, st.Name, Duration(st.Duration))
	if st.Error != "" {
		line += ": " + st.Error
	}
	r.endLine()
	r.println(line)
}

func outcome(rc *kernel.Receipt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s in %s", rc.Status, Duration(rc.Duration))
	if rc.Interruptions > 0 {
		fmt.Fprintf(&b, ", interrupted %s", times(rc.Interruptions))
	}
	if rc.Error != "" {
		b.WriteString(": " + rc.Error)
	}
	fmt.Fprintf(&b, "\nreceipt %s", rc.ID)
	return b.String()
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// running shows the current step's line. In a terminal it replaces the
// line already on screen.
func (r *Renderer) running(line string) {
	if !r.tty {
		r.println(line)
		return
	}
	if len(line) > r.width {
		line = line[:r.width-1] + "…"
	}
	fmt.Fprint(r.out, "\r\033[K"+line)
	r.live = true
}

// endLine clears a live line so the next print starts fresh.
func (r *Renderer) endLine() {
	if r.tty && r.live {
		fmt.Fprint(r.out, "\r\033[K")
		r.live = false
	}
}

func (r *Renderer) println(s string) {
	r.endLine()
	fmt.Fprintln(r.out, s)
}

// Plan prints a plan the way --plan shows it.
func (r *Renderer) Plan(view *kernel.PlanView) {
	if r.json {
		_ = r.enc.Encode(view)
		return
	}
	r.println(fmt.Sprintf("%s on %s: %d steps, %s on interruption", view.Kind, view.Target, len(view.Steps), recoveryWord(view.Recovery)))
	for _, st := range view.Steps {
		r.println(fmt.Sprintf("  %2d. %s", st.Index+1, orName(st.Change, st.Name)))
	}
	r.println("plan digest " + view.Digest)
}

func recoveryWord(rc kernel.Recovery) string {
	if rc == kernel.Compensate {
		return "undo"
	}
	return "resume"
}

func orName(change, name string) string {
	if change == "" {
		return name
	}
	return change
}

// Receipt prints a receipt in full.
func (r *Renderer) Receipt(rc *kernel.Receipt) {
	if r.json {
		_ = r.enc.Encode(rc)
		return
	}
	r.println(fmt.Sprintf("%s  %s on %s", rc.ID, rc.Kind, rc.Target))
	when := "not started"
	if !rc.StartedAt.IsZero() {
		when = rc.StartedAt.Local().Format("2006-01-02 15:04:05")
	}
	line := fmt.Sprintf("%s, %d steps, %s, started %s", rc.Status, len(rc.Steps), Duration(rc.Duration), when)
	if rc.Interruptions > 0 {
		line += fmt.Sprintf(", interrupted %s", times(rc.Interruptions))
	}
	if rc.Error != "" {
		line += "\n" + rc.Error
	}
	r.println(line)
	for _, st := range rc.Steps {
		word := map[state.StepStatus]string{state.StepPending: "-", state.StepRunning: "...", state.StepSucceeded: "ok", state.StepFailed: "fail", state.StepUndone: "undid"}[st.Status]
		s := fmt.Sprintf("  %-4s %s", word, orName(st.Change, st.Name))
		if st.Duration > 0 {
			s += fmt.Sprintf(" (%s)", Duration(st.Duration))
		}
		if st.Attempts > 1 {
			s += fmt.Sprintf(", %d attempts", st.Attempts)
		}
		if st.Error != "" {
			s += ": " + st.Error
		}
		r.println(s)
	}
}

// Summary is one row of history.
type Summary struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"`
	Target        string                `json:"target"`
	Status        state.OperationStatus `json:"status"`
	CreatedAt     time.Time             `json:"created_at"`
	Interruptions int                   `json:"interruptions"`
	Error         string                `json:"error,omitempty"`
}

// History prints operation summaries as a table, newest first.
func (r *Renderer) History(rows []Summary) {
	if r.json {
		_ = r.enc.Encode(rows)
		return
	}
	if len(rows) == 0 {
		r.println("no operations yet")
		return
	}
	w := tabwriter.NewWriter(r.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tTARGET\tSTATUS\tWHEN\tNOTE")
	for _, row := range rows {
		note := row.Error
		if row.Interruptions > 0 {
			note = fmt.Sprintf("interrupted %s; %s", times(row.Interruptions), note)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Kind, row.Target, row.Status, row.CreatedAt.Local().Format("2006-01-02 15:04"), strings.TrimSuffix(note, "; "))
	}
	w.Flush()
}

// Duration formats a duration the way people read it.
func Duration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}
