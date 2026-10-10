package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

func TestHistoryQueriesAndOutputMatchLocalAndDaemonRunners(t *testing.T) {
	ctx := context.Background()
	client, store := server(t)
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for i, row := range []struct {
		id, target string
		status     state.OperationStatus
	}{
		{"failed", "api", state.Failed},
		{"compensated", "api", state.Compensated},
		{"success", "api", state.Succeeded},
		{"unrelated", "site", state.Failed},
	} {
		op := state.NewOperation{ID: row.id, Kind: "app.deploy", Target: row.target, Input: json.RawMessage(`{}`), Plan: json.RawMessage(`{}`), StepNames: []string{"build-web"}, CreatedAt: start.Add(time.Duration(i) * time.Minute)}
		if err := store.CreateOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := store.StepFinished(ctx, row.id, 0, state.StepFailed, "build failed", "saved build detail\n", op.CreatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(ctx, row.id, row.status, "", nil, op.CreatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	local := &Local{Engine: kernel.New(store, kernel.Registry{}, "test-local"), Store: store}
	for _, runner := range []Runner{client, local} {
		rows, err := runner.ListFiltered(ctx, state.OperationFilter{Target: "api", Kind: "app.deploy", Failed: true, Since: start.Add(time.Minute), Limit: 1})
		if err != nil || len(rows) != 1 || rows[0].ID != "compensated" {
			t.Fatalf("filtered history: %v %+v", err, rows)
		}
		output, err := runner.Output(ctx, "failed", "build-web")
		if err != nil || len(output.Steps) != 1 || output.Steps[0].Output != "saved build detail\n" || output.TailLinesPerExecution != 40 {
			t.Fatalf("saved output: %v %+v", err, output)
		}
	}
}

func TestHistoryAPIRejectsBadFiltersAndStepSelectors(t *testing.T) {
	client, _ := server(t)
	for _, path := range []string{
		"/v1/operations?limit=oops",
		"/v1/operations?limit=-1",
		"/v1/operations?failed=maybe",
		"/v1/operations?since=yesterday",
	} {
		resp, err := client.http.Get(client.base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
	receipt, err := client.Run(context.Background(), kernel.ExerciseKind, input(t, t.TempDir(), 1, 0), func(kernel.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/v1/operations/" + receipt.ID + "/output?step=missing", http.StatusBadRequest},
		{"/v1/operations/missing/output", http.StatusNotFound},
	} {
		resp, err := client.http.Get(client.base + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: status %d", tc.path, resp.StatusCode)
		}
	}
}

func TestFilteredHistoryDoesNotSilentlyUseAnOlderDaemonsUnfilteredList(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operations" {
			writeJSON(w, http.StatusOK, []Summary{{ID: "unrelated", Target: "site"}})
			return
		}
		writeError(w, http.StatusNotFound, state.ErrNotFound)
	}))
	defer old.Close()
	client := NewClient(old.Client(), old.URL)
	defer client.Close()
	rows, err := client.List(context.Background(), 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ordinary history must remain compatible: %v %+v", err, rows)
	}
	rows, err = client.ListFiltered(context.Background(), state.OperationFilter{Target: "api", Limit: 20})
	if err == nil || len(rows) != 0 {
		t.Fatalf("old daemon filters were silently ignored: %v %+v", err, rows)
	}
}
