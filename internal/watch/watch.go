// Package watch keeps an eye on the machine and its apps and tells a
// person when something is wrong, once, and again when it is fixed. A
// condition has to hold for several rounds before it becomes an incident,
// so a blip never sends a mail, and an incident is one per subject, so an
// outage sends one notice however many checks it fails.
package watch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/state"
)

// Condition is one thing found wrong in a round.
type Condition struct {
	// Key names the subject: app:dragon-writer, host:disk, watch:<url>.
	Key      string
	Subject  string
	Severity string
	Message  string
}

// Notice is what a person gets told.
type Notice struct {
	Subject string
	Body    string
}

// Notifier delivers notices.
type Notifier interface {
	Notify(ctx context.Context, n Notice) error
}

// Defaults for the watcher's patience.
const (
	DefaultOpenAfter    = 3
	DefaultResolveAfter = 2
	DefaultRenotify     = 24 * time.Hour
)

// Watcher turns rounds of conditions into incidents and notices.
type Watcher struct {
	Store *state.Store
	// Notifier tells a person; nil records incidents and tells no one.
	Notifier Notifier
	Hostname string
	Now      func() time.Time
	// OpenAfter is how many rounds in a row a condition must hold before
	// an incident opens; ResolveAfter how many clean rounds close it.
	OpenAfter    int
	ResolveAfter int
	// Renotify is how often an open incident is repeated.
	Renotify time.Duration
	Log      func(format string, args ...any)

	failing map[string]int
	passing map[string]int
}

func (w *Watcher) init() {
	if w.failing == nil {
		w.failing = map[string]int{}
		w.passing = map[string]int{}
	}
	if w.Now == nil {
		w.Now = func() time.Time { return time.Now().UTC() }
	}
	if w.OpenAfter <= 0 {
		w.OpenAfter = DefaultOpenAfter
	}
	if w.ResolveAfter <= 0 {
		w.ResolveAfter = DefaultResolveAfter
	}
	if w.Renotify <= 0 {
		w.Renotify = DefaultRenotify
	}
	if w.Log == nil {
		w.Log = func(string, ...any) {}
	}
}

// Round takes this round's conditions and updates incidents: opens the
// ones that held long enough, resolves the ones that cleared, and
// notifies about both.
func (w *Watcher) Round(ctx context.Context, conditions []Condition) error {
	w.init()
	now := w.Now()
	current := map[string]Condition{}
	for _, c := range conditions {
		current[c.Key] = c
	}
	open, err := w.Store.OpenIncidents(ctx)
	if err != nil {
		return err
	}
	openByKey := map[string]state.Incident{}
	for _, inc := range open {
		openByKey[inc.Key] = inc
	}

	for key, c := range current {
		w.failing[key]++
		w.passing[key] = 0
		inc, isOpen := openByKey[key]
		if !isOpen {
			if w.failing[key] < w.OpenAfter {
				continue
			}
			inc = state.Incident{Key: key, Subject: c.Subject, Severity: c.Severity, Message: c.Message, OpenedAt: now, Observations: w.failing[key]}
			id, err := w.Store.OpenIncident(ctx, inc)
			if err != nil {
				return err
			}
			inc.ID = id
			w.Log("alert: %s: %s", c.Subject, c.Message)
			w.notify(ctx, &inc, w.openNotice(inc), now)
			if err := w.Store.UpdateIncident(ctx, inc); err != nil {
				return err
			}
			continue
		}
		inc.Observations++
		inc.Message, inc.Severity = c.Message, c.Severity
		if inc.NotifiedAt.IsZero() || now.Sub(inc.NotifiedAt) >= w.Renotify {
			w.notify(ctx, &inc, w.openNotice(inc), now)
		}
		if err := w.Store.UpdateIncident(ctx, inc); err != nil {
			return err
		}
	}

	for key, inc := range openByKey {
		if _, still := current[key]; still {
			continue
		}
		w.passing[key]++
		w.failing[key] = 0
		if w.passing[key] < w.ResolveAfter {
			continue
		}
		inc.ResolvedAt = now
		w.Log("recovered: %s", inc.Subject)
		w.notify(ctx, &inc, w.resolvedNotice(inc), now)
		if err := w.Store.UpdateIncident(ctx, inc); err != nil {
			return err
		}
		delete(w.passing, key)
	}

	for key := range w.failing {
		if _, ok := current[key]; !ok {
			if _, ok := openByKey[key]; !ok {
				delete(w.failing, key)
				delete(w.passing, key)
			}
		}
	}
	return nil
}

// notify sends a notice and records the outcome on the incident. A
// failed delivery is kept as the incident's notify error and tried again
// next round.
func (w *Watcher) notify(ctx context.Context, inc *state.Incident, n Notice, now time.Time) {
	if w.Notifier == nil {
		inc.NotifyError = "no way to send alerts is set up; run bedrock integration set email"
		return
	}
	if err := w.Notifier.Notify(ctx, n); err != nil {
		inc.NotifyError = err.Error()
		w.Log("alert not delivered: %v", err)
		return
	}
	inc.NotifiedAt = now
	inc.NotifyError = ""
}

func (w *Watcher) openNotice(inc state.Incident) Notice {
	word := "has a problem"
	if inc.Severity == state.SeverityCritical {
		word = "is down"
	}
	subject := fmt.Sprintf("%s: %s %s", w.Hostname, inc.Subject, word)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", inc.Message)
	fmt.Fprintf(&b, "Since %s (%s), seen in %d rounds.\n", inc.OpenedAt.Format("2006-01-02 15:04 UTC"), ago(inc.OpenedAt, w.Now()), inc.Observations)
	fmt.Fprintf(&b, "Machine: %s\n\nbedrock status and bedrock alerts on the machine show more. You'll hear again when it recovers, or in %s if it doesn't.\n", w.Hostname, w.Renotify)
	return Notice{Subject: subject, Body: b.String()}
}

func (w *Watcher) resolvedNotice(inc state.Incident) Notice {
	subject := fmt.Sprintf("%s: %s recovered", w.Hostname, inc.Subject)
	body := fmt.Sprintf("Recovered after %s.\n\nIt was: %s\n\nMachine: %s\n", ago(inc.OpenedAt, inc.ResolvedAt), inc.Message, w.Hostname)
	return Notice{Subject: subject, Body: body}
}

// ago says how long before now a time was, roughly.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
