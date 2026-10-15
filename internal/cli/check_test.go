package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

func TestCheckIsOfflineAndDoesNotCreateState(t *testing.T) {
	dir := t.TempDir()
	name := "helios.worker.yaml"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("app: mailer\nworkloads: {worker: {kind: worker, image: alpine}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "absent-state")
	out, errOut, code := run(t, stateDir, "check", dir, "--manifest", name)
	if code != 0 || errOut != "" || !strings.Contains(out, "mailer manifest and local source inputs are valid") || !strings.Contains(out, "Offline check only") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("check opened or created state: %v", err)
	}
}

func TestCheckJSONReportsInvalidFieldsAndExitsNonzero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte("app: mailer\nworkloads:\n  worker: {kind: worker, image: alpine, colour: blue}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, t.TempDir(), "--json", "check", dir)
	if code != 1 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	var report manifest.CheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if report.Valid || report.SchemaVersion != manifest.SchemaVersion || len(report.Diagnostics) != 1 || report.Diagnostics[0].Line != 3 || report.Diagnostics[0].Path != "workloads.worker.colour" {
		t.Fatalf("%+v", report)
	}
}

func TestCheckJSONReportsMissingManifestsWithoutMixedOutput(t *testing.T) {
	out, errOut, code := run(t, t.TempDir(), "--json", "check", t.TempDir())
	if code != 1 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	var report manifest.CheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.Valid || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "manifest.unreadable" {
		t.Fatalf("%v %+v: %q", err, report, out)
	}
}

func TestHostCheckUsesTheDeployPlanAndRecordsNoOperation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte("app: mailer\nworkloads: {worker: {kind: worker, image: alpine}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	out, errOut, code := run(t, stateDir, "--json", "check", dir, "--host")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	var report sourceCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || !report.Valid || report.HostPreflight == nil || !report.HostPreflight.Valid || report.HostPreflight.Plan == nil || report.HostPreflight.Plan.Kind != "app.deploy" {
		t.Fatalf("%v %+v: %q", err, report, out)
	}
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operations, err := store.List(context.Background(), 20)
	if err != nil || len(operations) != 0 {
		t.Fatalf("host check changed the operation journal: %v %v", operations, err)
	}
	revisions, err := store.Revisions(context.Background(), "mailer")
	if err != nil || len(revisions) != 0 {
		t.Fatalf("host check recorded a revision: %v %v", revisions, err)
	}
}

func TestHostCheckDoesNotContactTheTargetUntilOfflineInputsPass(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "absent-state")
	out, _, code := run(t, stateDir, "--json", "check", t.TempDir(), "--host")
	var report sourceCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 1 || report.HostPreflight != nil {
		t.Fatalf("%v, code %d, report %+v", err, code, report)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("host check opened state after the offline check failed: %v", err)
	}
}

func TestHostCheckRefusesCrossAppRouteOwnership(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	body := "app: site\nworkloads: {web: {kind: web, image: alpine, port: 8080, routes: [{host: shared.example.com}]}}\n"
	m, err := manifest.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRevision(ctx, state.Revision{App: "site", ID: "r1", Status: state.RevisionActive, Manifest: raw, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(strings.Replace(body, "app: site", "app: other", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, stateDir, "--json", "check", dir, "--host")
	var report sourceCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 1 || errOut != "" || report.Valid || report.HostPreflight == nil || report.HostPreflight.Valid {
		t.Fatalf("%v, code %d, report %+v, stderr %q", err, code, report, errOut)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "host.preflight" || !strings.Contains(report.Diagnostics[0].Message, "already routed to site's web") {
		t.Fatalf("%+v", report.Diagnostics)
	}
}

func TestHostCheckRefusesMissingSecretsWithoutCreatingSecretState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte("app: mailer\nworkloads: {worker: {kind: worker, image: alpine, secrets: [API_TOKEN]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	out, errOut, code := run(t, stateDir, "--json", "check", dir, "--host")
	var report sourceCheckReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 1 || errOut != "" || report.Valid || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, "API_TOKEN") {
		t.Fatalf("%v, code %d, report %+v, stderr %q", err, code, report, errOut)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "secrets")); !os.IsNotExist(err) {
		t.Fatalf("secret preflight wrote secrets: %v", err)
	}
}
