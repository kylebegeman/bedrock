package watch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/state"
)

type fakeNotifier struct {
	sent []Notice
	fail bool
}

func (f *fakeNotifier) Notify(_ context.Context, n Notice) error {
	if f.fail {
		return errors.New("mail server down")
	}
	f.sent = append(f.sent, n)
	return nil
}

func newWatcher(t *testing.T, n Notifier) (*Watcher, *time.Time) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	w := &Watcher{Store: store, Notifier: n, Hostname: "box", Now: func() time.Time { return now }}
	return w, &now
}

func down(app string) Condition {
	return Condition{Key: "app:" + app, Subject: app, Severity: state.SeverityCritical, Message: "web isn't running"}
}

func TestAnIncidentOpensAfterThreeRoundsAndResolvesAfterTwo(t *testing.T) {
	ctx := context.Background()
	n := &fakeNotifier{}
	w, now := newWatcher(t, n)
	for i := 0; i < 2; i++ {
		if err := w.Round(ctx, []Condition{down("hello")}); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	if open, _ := w.Store.OpenIncidents(ctx); len(open) != 0 || len(n.sent) != 0 {
		t.Fatalf("too eager: %d open, %d sent", len(open), len(n.sent))
	}
	if err := w.Round(ctx, []Condition{down("hello")}); err != nil {
		t.Fatal(err)
	}
	open, _ := w.Store.OpenIncidents(ctx)
	if len(open) != 1 || len(n.sent) != 1 {
		t.Fatalf("expected one incident and one notice: %d open, %d sent", len(open), len(n.sent))
	}
	if !strings.Contains(n.sent[0].Subject, "hello is down") || !strings.Contains(n.sent[0].Body, "web isn't running") {
		t.Fatalf("notice: %+v", n.sent[0])
	}
	// Still down: no second notice within a day.
	for i := 0; i < 5; i++ {
		*now = now.Add(time.Minute)
		_ = w.Round(ctx, []Condition{down("hello")})
	}
	if len(n.sent) != 1 {
		t.Fatalf("repeated too soon: %d notices", len(n.sent))
	}
	// One clean round isn't recovery yet.
	*now = now.Add(time.Minute)
	_ = w.Round(ctx, nil)
	if open, _ := w.Store.OpenIncidents(ctx); len(open) != 1 {
		t.Fatal("resolved after one clean round")
	}
	*now = now.Add(time.Minute)
	_ = w.Round(ctx, nil)
	if open, _ := w.Store.OpenIncidents(ctx); len(open) != 0 {
		t.Fatal("not resolved after two clean rounds")
	}
	if len(n.sent) != 2 || !strings.Contains(n.sent[1].Subject, "hello recovered") {
		t.Fatalf("recovery notice: %+v", n.sent)
	}
	all, _ := w.Store.Incidents(ctx, 10)
	if len(all) != 1 || all[0].Open() || all[0].Observations != 8 {
		t.Fatalf("history: %+v", all)
	}
}

func TestABlipNeverBecomesAnIncident(t *testing.T) {
	ctx := context.Background()
	n := &fakeNotifier{}
	w, now := newWatcher(t, n)
	for i := 0; i < 6; i++ {
		var conds []Condition
		if i%2 == 0 {
			conds = []Condition{down("hello")}
		}
		_ = w.Round(ctx, conds)
		*now = now.Add(time.Minute)
	}
	if open, _ := w.Store.OpenIncidents(ctx); len(open) != 0 || len(n.sent) != 0 {
		t.Fatal("a flapping condition opened an incident")
	}
}

func TestAFailedNoticeIsTriedAgainNextRound(t *testing.T) {
	ctx := context.Background()
	n := &fakeNotifier{fail: true}
	w, now := newWatcher(t, n)
	for i := 0; i < 3; i++ {
		_ = w.Round(ctx, []Condition{down("hello")})
		*now = now.Add(time.Minute)
	}
	open, _ := w.Store.OpenIncidents(ctx)
	if len(open) != 1 || open[0].NotifyError == "" || !open[0].NotifiedAt.IsZero() {
		t.Fatalf("expected an open incident with a delivery error: %+v", open)
	}
	n.fail = false
	_ = w.Round(ctx, []Condition{down("hello")})
	open, _ = w.Store.OpenIncidents(ctx)
	if len(n.sent) != 1 || open[0].NotifyError != "" || open[0].NotifiedAt.IsZero() {
		t.Fatalf("not retried: sent %d, %+v", len(n.sent), open[0])
	}
}

func TestAnOpenIncidentIsRepeatedAfterADay(t *testing.T) {
	ctx := context.Background()
	n := &fakeNotifier{}
	w, now := newWatcher(t, n)
	for i := 0; i < 3; i++ {
		_ = w.Round(ctx, []Condition{down("hello")})
		*now = now.Add(time.Minute)
	}
	*now = now.Add(25 * time.Hour)
	c := down("hello")
	c.Message = "web isn't running; https://hello.example/ answers connection refused"
	_ = w.Round(ctx, []Condition{c})
	if len(n.sent) != 2 || !strings.Contains(n.sent[1].Body, "connection refused") {
		t.Fatalf("expected a repeat with the latest message: %+v", n.sent)
	}
}

func TestWithoutANotifierIncidentsAreStillRecorded(t *testing.T) {
	ctx := context.Background()
	w, now := newWatcher(t, nil)
	for i := 0; i < 3; i++ {
		_ = w.Round(ctx, []Condition{{Key: "host:disk", Subject: "disk", Severity: state.SeverityCritical, Message: "2 GB free"}})
		*now = now.Add(time.Minute)
	}
	open, _ := w.Store.OpenIncidents(ctx)
	if len(open) != 1 || !strings.Contains(open[0].NotifyError, "integration set email") {
		t.Fatalf("%+v", open)
	}
}
