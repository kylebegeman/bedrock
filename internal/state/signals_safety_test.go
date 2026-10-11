package state

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSignalStoreRejectsNonFiniteValues(t *testing.T) {
	for _, method := range []string{"add", "max"} {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(method, func(t *testing.T) {
				s := open(t)
				sig := Signal{Hour: time.Now(), App: "hello", Key: "web", Metric: SignalCPUPercent, Value: value, Samples: 1}
				var err error
				if method == "add" {
					err = s.AddSignal(context.Background(), sig)
				} else {
					err = s.MaxSignal(context.Background(), sig)
				}
				if err == nil || !strings.Contains(err.Error(), "finite") {
					t.Fatalf("invalid signal %v: %v", value, err)
				}
				rows, err := s.Signals(context.Background(), "hello", time.Time{})
				if err != nil || len(rows) != 0 {
					t.Fatalf("invalid signal persisted: %v %v", rows, err)
				}
			})
		}
	}
}
func TestPendingResolvedIncidentNoticesSelectOnlyFailedRecoveries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, row := range []struct {
		key      string
		resolved bool
		failed   bool
	}{{"open", false, true}, {"delivered", true, false}, {"pending", true, true}} {
		incident := Incident{Key: row.key, OpenedAt: now}
		id, err := s.OpenIncident(ctx, incident)
		if err != nil {
			t.Fatal(err)
		}
		incident.ID = id
		if row.resolved {
			incident.ResolvedAt = now.Add(time.Minute)
		}
		if row.failed {
			incident.NotifyError = "delivery failed"
		}
		if err := s.UpdateIncident(ctx, incident); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.PendingResolvedIncidentNotices(ctx)
	if err != nil || len(rows) != 1 || rows[0].Key != "pending" {
		t.Fatalf("pending recovery notices: %+v %v", rows, err)
	}
	rows[0].NotifyError = ""
	if err := s.UpdateIncident(ctx, rows[0]); err != nil {
		t.Fatal(err)
	}
	rows, err = s.PendingResolvedIncidentNotices(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("delivered notice remained pending: %+v %v", rows, err)
	}
}

func TestSignalRollupRejectsOverflowWithoutChangingSavedValue(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	sig := Signal{Hour: time.Now(), App: "hello", Key: "web", Metric: SignalCPUPercent, Value: math.MaxFloat64, Samples: 1}
	if err := s.AddSignal(ctx, sig); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSignal(ctx, sig); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Fatalf("overflow accepted: %v", err)
	}
	rows, err := s.Signals(ctx, "hello", time.Time{})
	if err != nil || len(rows) != 1 || rows[0].Value != math.MaxFloat64 || rows[0].Samples != 1 {
		t.Fatalf("overflow changed rollup: %+v %v", rows, err)
	}
}
