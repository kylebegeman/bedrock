package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/state"
)

func TestAlertsAlwaysIncludeOpenIncidentsBeyondHistoryLimit(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		_, err := store.OpenIncident(ctx, state.Incident{Key: fmt.Sprintf("app:down-%d", i), Subject: fmt.Sprintf("down-%d", i), Message: "web stopped", Severity: state.SeverityCritical, OpenedAt: start.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		inc := state.Incident{Key: fmt.Sprintf("app:recovered-%d", i), Subject: fmt.Sprintf("recovered-%d", i), Message: "recovered", OpenedAt: start.Add(time.Duration(i+10) * time.Minute)}
		inc.ID, err = store.OpenIncident(ctx, inc)
		if err != nil {
			t.Fatal(err)
		}
		inc.ResolvedAt = inc.OpenedAt.Add(time.Minute)
		if err := store.UpdateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, code := run(t, stateDir, "alerts", "--limit", "2")
	if code != 0 || strings.Contains(out, "nothing wrong right now") || strings.Count(out, "OPEN  ") != 3 || strings.Count(out, "past  ") != 2 {
		t.Fatalf("open alerts must survive history limits: code %d, stderr %q, output %q", code, errOut, out)
	}
	out, errOut, code = run(t, stateDir, "--json", "alerts", "--limit", "2")
	var incidents []state.Incident
	if code != 0 || json.Unmarshal([]byte(out), &incidents) != nil || len(incidents) != 5 {
		t.Fatalf("JSON must retain the incident array and all open incidents: code %d, stderr %q, output %q", code, errOut, out)
	}
	out, _, code = run(t, stateDir, "alerts", "--limit", "0")
	if code != 0 || strings.Count(out, "OPEN  ") != 3 || strings.Contains(out, "past  ") {
		t.Fatalf("zero history still shows all open alerts: code %d, output %q", code, out)
	}
}
