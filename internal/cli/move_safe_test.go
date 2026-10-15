package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	apps "github.com/kylebegeman/helios/internal/app"
	"github.com/kylebegeman/helios/internal/docker"
	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

func sourceForSafeMove(t *testing.T) (string, *state.Store, *manifest.Manifest) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := &manifest.Manifest{App: "site", Workloads: map[string]manifest.Workload{"web": {Kind: manifest.Web, Image: "site:old", Port: 80, Routes: []manifest.Route{{Host: "site.example.com"}}}}}
	data, _ := json.Marshal(m)
	if err := store.SaveRevision(context.Background(), state.Revision{App: m.App, ID: "source-r1", Status: state.RevisionActive, Manifest: data, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return dir, store, m
}

func TestMoveFinalPlansOnePinnedFenceWithoutStoppingTheSource(t *testing.T) {
	dir, store, _ := sourceForSafeMove(t)
	out, errOut, code := run(t, dir, "move", "final", "site", "--to", "target-name", "--plan")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	for _, text := range []string{"app.move.capture", "source-r1", "target-name", "fence", "handoff"} {
		if !strings.Contains(out, text) {
			t.Fatalf("plan omitted %s: %s", text, out)
		}
	}
	if err := apps.CheckMigrationAccess(store.Path(), "site"); err != nil {
		t.Fatal("plan created a live fence")
	}
}

func TestMoveFinalKeepsStdoutForFinalHandoffAndResumeIsDeliberate(t *testing.T) {
	dir, store, m := sourceForSafeMove(t)
	fake := dockertest.New(t)
	writer := dockertest.Container{Name: "bedrock-site-web-source-r1", Running: true, Labels: map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: m.App, docker.LabelWorkload: "web"}}
	writer.HostConfig.RestartPolicy.Name = "unless-stopped"
	fake.Add(writer)
	out, errOut, code := run(t, dir, "move", "final", "site", "--to", "target-name", "--yes")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	h, err := apps.ReadHandoff(strings.NewReader(out))
	if err != nil {
		t.Fatalf("stdout mixed narrative with handoff: %v %s", err, out)
	}
	if h.Phase != "final" || h.To != "target-name" || h.Revision != "source-r1" || h.Migration == "" {
		t.Fatalf("handoff lost binding: %+v", h)
	}
	if err := apps.CheckMigrationAccess(store.Path(), m.App); err == nil {
		t.Fatal("final source remains writable")
	}
	_, errOut, code = run(t, dir, "move", "resume", "site", "--yes")
	if code == 0 || !strings.Contains(errOut, "target-stopped") {
		t.Fatalf("implicit resume: %d %s", code, errOut)
	}
	_, errOut, code = run(t, dir, "move", "resume", "site", "--target-stopped", "--yes")
	if code != 0 {
		t.Fatalf("resume %d %s", code, errOut)
	}
	if c, _ := fake.Container(writer.Name); !c.Running || c.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Fatal("deliberate resume did not restore writer")
	}
	input, err := apps.MoveCaptureInputFor(context.Background(), store, m.App, thisMachine(), "target-name", true)
	if err != nil {
		t.Fatal(err)
	}
	if input.Migration == h.Migration {
		t.Fatal("new final capture reused a deliberately resumed transfer identity")
	}
}

func TestMoveStageCLIPlanKeepsTargetRevisionStableForApproval(t *testing.T) {
	sourceState, store, m := sourceForSafeMove(t)
	dockertest.New(t)
	out, errOut, code := run(t, sourceState, "move", "final", m.App, "--to", thisMachine(), "--yes")
	// The real hostname cannot be both ends; source fixture has no remote
	// source machine, so ask a distinct target then retain the captured proof.
	if code == 0 {
		t.Fatal("capture allowed equal source and destination")
	}
	out, errOut, code = run(t, sourceState, "move", "final", m.App, "--to", "captured-target", "--yes")
	if code != 0 {
		t.Fatalf("capture %d %s %s", code, out, errOut)
	}
	h, err := apps.ReadMoveHandoff(store.Path(), m.App, "final")
	if err != nil {
		t.Fatal(err)
	}
	h.To = thisMachine()
	h.From = "source-fixture"
	source := t.TempDir()
	data, _ := yaml.Marshal(m)
	if err := os.WriteFile(filepath.Join(source, manifest.FileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(t.TempDir(), "final.json")
	body, _ := json.Marshal(h)
	if err := os.WriteFile(handoff, body, 0600); err != nil {
		t.Fatal(err)
	}
	targetState := t.TempDir()
	first, errOut, code := run(t, targetState, "move", "stage", source, "--handoff", handoff, "--plan")
	if code != 0 {
		t.Fatalf("stage %d %s %s", code, first, errOut)
	}
	second, errOut, code := run(t, targetState, "move", "stage", source, "--handoff", handoff, "--plan")
	if code != 0 {
		t.Fatalf("repeat %d %s", code, errOut)
	}
	if first != second {
		t.Fatalf("approval plan changed between invocations:\n%s\n%s", first, second)
	}
	if !strings.Contains(first, apps.MoveTargetRevision(*h)) {
		t.Fatal("plan failed to name pinned target revision")
	}
}
