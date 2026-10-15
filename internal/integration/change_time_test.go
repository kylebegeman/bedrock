package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/secrets"
)

func TestIntegrationStatusTracksValueChangesAndCommittedVersion(t *testing.T) {
	store := newStore(t)
	if _, _, err := Set(store, CloudflareName, map[string]string{"token": "first"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Set(store, EmailName, map[string]string{"smtp_host": "smtp.invalid", "from": "a@example.com", "to": "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Set(store, CloudflareName, map[string]string{"token": "changed"}); err != nil {
		t.Fatal(err)
	}
	times := []time.Time{time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 1, 0, 0, 0, time.UTC)}
	versions, err := store.Versions(App)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range versions {
		info.At = times[info.Version-1]
		writeVersionMeta(t, store, info)
	}
	writeVersionMeta(t, store, secrets.VersionInfo{Version: 4, At: times[2].Add(time.Hour), Names: []string{"CLOUDFLARE_TOKEN", "CLOUDFLARE_API"}})
	statuses, err := List(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Name == CloudflareName && !status.At.Equal(times[2]) {
			t.Errorf("token update timestamp = %s, want %s", status.At, times[2])
		}
		if status.Name == EmailName && !status.At.Equal(times[1]) {
			t.Errorf("unrelated updates changed email timestamp = %s", status.At)
		}
	}
}

func writeVersionMeta(t *testing.T, store *secrets.Store, info secrets.VersionInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, App, fmt.Sprintf("v%d.meta", info.Version))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
