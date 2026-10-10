package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylebegeman/bedrock/internal/compat"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

func TestCapabilityEndpointIsAdditiveAndDoesNotCreateASecretsKey(t *testing.T) {
	client, store := server(t)
	info, err := client.Capabilities(context.Background())
	if err != nil || info.APIVersion != 1 || info.ManifestVersion != 1 || info.StateVersion != state.SchemaVersion || info.MachineID == "" || info.PersistedStateVersion == nil || *info.PersistedStateVersion != state.SchemaVersion {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Path()), "secrets.key")); !os.IsNotExist(err) {
		t.Fatal("capability query created a secrets key")
	}
	operations, _ := store.List(context.Background(), 10)
	if len(operations) != 0 {
		t.Fatal("capability query journaled an operation")
	}
}
func TestAPIErrorCodesPreserveExistingMessagesAndRetryPolicy(t *testing.T) {
	client, _ := server(t)
	ctx := context.Background()
	_, err := client.Receipt(ctx, "absent")
	var problem *Error
	if !errors.As(err, &problem) || problem.Code != "not_found" || problem.Retryable || problem.APIVersion != 1 || err.Error() != state.ErrNotFound.Error() {
		t.Fatalf("%+v %v", problem, err)
	}
	_, err = client.RunExpecting(ctx, kernel.ExerciseKind, input(t, t.TempDir(), 1, 0), "wrong-digest", func(kernel.Event) {})
	if !errors.As(err, &problem) || problem.Code != "plan_changed" || problem.Retryable {
		t.Fatalf("stream problem: %+v %v", problem, err)
	}
	lease := problemFor(http.StatusConflict, state.ErrLeased)
	if lease.Code != "operation_leased" || !lease.Retryable {
		t.Fatalf("%+v", lease)
	}
	unavailable := problemFor(http.StatusServiceUnavailable, errors.New("maintenance"))
	if unavailable.Code != "unavailable" || !unavailable.Retryable {
		t.Fatalf("%+v", unavailable)
	}
}
func TestClientAcceptsLegacyStringErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"legacy error"}`))
	}))
	defer ts.Close()
	_, err := NewClient(ts.Client(), ts.URL).Capabilities(context.Background())
	var problem *Error
	if !errors.As(err, &problem) || err.Error() != "legacy error" || problem.Code != "" {
		t.Fatalf("%+v %v", problem, err)
	}
	if compat.APIVersion != 1 {
		t.Fatal("API test contract changed")
	}
}
