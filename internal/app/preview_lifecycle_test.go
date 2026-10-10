package app

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

func lifecyclePreview(t *testing.T, store *state.Store, name, of, branch, revision string, at time.Time, pinned bool) PreviewTarget {
	t.Helper()
	m := &manifest.Manifest{App: name, Preview: &manifest.Preview{Of: of, Branch: branch, Pinned: pinned}, Workloads: map[string]manifest.Workload{"web": {Kind: manifest.Web, Image: "image", Port: 80, Routes: []manifest.Route{{Host: name + ".example.com", Auth: manifest.AuthLoom}}}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRevision(context.Background(), state.Revision{App: name, ID: revision, Status: state.RevisionActive, Manifest: raw, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	return PreviewTarget{App: name, Revision: revision, Of: of, Branch: branch}
}

func TestPreviewSelectionExcludesProductionPinnedAndRecentlyDeployedApps(t *testing.T) {
	store := goldenStore(t)
	now := time.Now().UTC()
	old := now.Add(-14 * 24 * time.Hour)
	want := lifecyclePreview(t, store, "site-pr-old", "site", "feature/old", "r1", old, false)
	lifecyclePreview(t, store, "site-pr-pinned", "site", "pinned", "r1", old, true)
	lifecyclePreview(t, store, "site-pr-new", "site", "new", "r1", now, false)
	lifecyclePreview(t, store, "api-pr-old", "api", "old", "r1", old, false)
	seedApp(t, store, "site-pr-production")
	selected, err := PrunablePreviews(context.Background(), store, "site", now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected, []PreviewTarget{want}) {
		t.Fatalf("selected %+v", selected)
	}
	resolved, err := ResolvePreview(context.Background(), store, "site", "feature/old")
	if err != nil || resolved.App != want.App {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	if _, err := ResolvePreview(context.Background(), store, "site-pr-production", ""); err == nil {
		t.Fatal("ordinary app mistaken for a preview")
	}
	if _, err := ResolvePreview(context.Background(), store, "site", "feature-old"); err == nil {
		t.Fatal("normalized branch was treated as the exact metadata branch")
	}
}

func TestPinningChangesMetadataOnlyAndProtectsAnAlreadyPlannedPrune(t *testing.T) {
	ctx := context.Background()
	store := goldenStore(t)
	target := lifecyclePreview(t, store, "site-pr-old", "site", "old", "r1", time.Now().Add(-14*24*time.Hour), false)
	remove := Remove{Store: store, Secrets: newSecrets(t), StateDir: t.TempDir()}
	prune := PreviewPrune{Remove: remove}
	raw, _ := json.Marshal(PreviewPruneInput{Previews: []PreviewTarget{target}})
	plan, err := prune.Plan(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	pin := PreviewPin{Store: store}
	in, _ := json.Marshal(PreviewPinInput{PreviewTarget: target, Pinned: true})
	pinPlan, err := pin.Plan(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.GetRevision(ctx, target.App, target.Revision)
	if err := pinPlan.Steps[0].Apply(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetRevision(ctx, target.App, target.Revision)
	if before.CreatedAt != after.CreatedAt || after.Status != state.RevisionActive {
		t.Fatal("pinning changed app lifecycle")
	}
	if err := plan.Steps[0].Apply(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("stale prune ignored pin: %v", err)
	}
	if _, err := prune.Plan(ctx, raw); err == nil {
		t.Fatal("pinned preview planned for pruning")
	}
	explicit, _ := json.Marshal(PreviewPruneInput{Previews: []PreviewTarget{target}, Explicit: true})
	if _, err := prune.Plan(ctx, explicit); err != nil {
		t.Fatalf("explicit down was blocked by pin: %v", err)
	}
}

func TestPruneUsesRemoveAndCanRepeatAfterACompletedOrPartialRemoval(t *testing.T) {
	ctx := context.Background()
	fake := dockertest.New(t)
	store := goldenStore(t)
	sec := newSecrets(t)
	if _, _, err := integration.Set(sec, integration.LoomName, map[string]string{"verify_url": "https://loom.example.com/api/cloud/auth/verify"}); err != nil {
		t.Fatal(err)
	}
	target := lifecyclePreview(t, store, "site-pr-old", "site", "old", "r1", time.Now().Add(-14*24*time.Hour), false)
	lifecyclePreview(t, store, "site-pr-kept", "site", "kept", "r1", time.Now(), true)
	for _, appName := range []string{target.App, "site-pr-kept"} {
		if _, err := sec.Set(appName, "TOKEN", "private-value"); err != nil {
			t.Fatal(err)
		}
		fake.Add(dockertest.Container{Name: "bedrock-" + appName + "-web-r1", Running: true, Labels: map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: appName, docker.LabelWorkload: "web"}})
		fake.AddVolume(docker.VolumeName(appName, "postgres"), map[string]string{docker.LabelOwner: docker.OwnerValue})
		fake.AddNetwork(docker.AppNetwork(appName), map[string]string{docker.LabelOwner: docker.OwnerValue})
	}
	remove := Remove{Store: store, Secrets: sec, StateDir: t.TempDir()}
	prune := PreviewPrune{Remove: remove}
	raw, _ := json.Marshal(PreviewPruneInput{Previews: []PreviewTarget{target}})
	reg := kernel.Registry{}
	reg.Add(prune)
	engine := kernel.New(store, reg, "test")
	before, err := engine.PlanOnly(ctx, PreviewPruneKind, raw)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after unroute but before the remaining removal.
	removeInput, _ := json.Marshal(RemoveInput{App: target.App, Data: true})
	partial, err := remove.Plan(ctx, removeInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := partial.Steps[0].Apply(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	resumed, err := engine.PlanOnly(ctx, PreviewPruneKind, raw)
	if err != nil || resumed.Digest != before.Digest {
		t.Fatalf("partial prune changed plan: %v %+v", err, resumed)
	}
	receipt, err := engine.RunExpecting(ctx, PreviewPruneKind, raw, before.Digest, func(kernel.Event) {})
	if err != nil || receipt.Status != state.Succeeded {
		t.Fatalf("prune: %v %+v", err, receipt)
	}
	if revs, _ := store.Revisions(ctx, target.App); len(revs) != 0 {
		t.Fatal("preview revisions survived")
	}
	if _, ok := fake.Container("bedrock-" + target.App + "-web-r1"); ok {
		t.Fatal("preview container survived")
	}
	if current, _ := sec.Current(target.App); current != 0 {
		t.Fatal("preview secrets survived")
	}
	if _, ok := fake.Container("bedrock-site-pr-kept-web-r1"); !ok {
		t.Fatal("unselected preview was removed")
	}
	if values, _, err := sec.LoadCurrent("site-pr-kept"); err != nil || values["TOKEN"] != "private-value" {
		t.Fatal("unselected preview secrets changed")
	}
	done, err := engine.PlanOnly(ctx, PreviewPruneKind, raw)
	if err != nil || done.Digest != before.Digest {
		t.Fatalf("completed prune changed recovery plan: %v", err)
	}
	plan, err := prune.Plan(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Steps[0].Apply(ctx, io.Discard); err != nil {
		t.Fatalf("repeat: %v", err)
	}
}

func TestPruneRefusesAReplacedRevisionBeforeAnyRemoval(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := lifecyclePreview(t, store, "site-pr-old", "site", "old", "r1", time.Now().Add(-14*24*time.Hour), false)
	prune := PreviewPrune{Remove: Remove{Store: store, Secrets: newSecrets(t), StateDir: t.TempDir()}}
	raw, _ := json.Marshal(PreviewPruneInput{Previews: []PreviewTarget{target}})
	plan, err := prune.Plan(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetRevisionStatus(ctx, target.App, "r1", state.RevisionPrevious); err != nil {
		t.Fatal(err)
	}
	lifecyclePreview(t, store, target.App, "site", "old", "r2", time.Now(), false)
	if err := plan.Steps[0].Apply(ctx, io.Discard); err == nil || !strings.Contains(err.Error(), "redeployed") {
		t.Fatalf("newer preview removed: %v", err)
	}
	if current, _ := store.RevisionWithStatus(ctx, target.App, state.RevisionActive); current == nil || current.ID != "r2" {
		t.Fatal("new revision changed")
	}
}

func TestPreviewAgeAcceptsDaysAndRefusesInvalidOrOverflowingDurations(t *testing.T) {
	for input, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, "48h": 48 * time.Hour} {
		got, err := ParsePreviewAge(input)
		if err != nil || got != want {
			t.Fatalf("%s: %s %v", input, got, err)
		}
	}
	for _, input := range []string{"0d", "-1d", "later", "900000000d", "3000000h"} {
		if _, err := ParsePreviewAge(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestPinRetentionSurvivesRollbackOfTheSamePreview(t *testing.T) {
	ctx := context.Background()
	store := goldenStore(t)
	at := time.Now().UTC().Add(-24 * time.Hour)
	older := lifecyclePreview(t, store, "site-pr-nav", "site", "nav", "r1", at, false)
	if err := store.SetRevisionStatus(ctx, older.App, older.Revision, state.RevisionPrevious); err != nil {
		t.Fatal(err)
	}
	current := lifecyclePreview(t, store, older.App, "site", "nav", "r2", at.Add(time.Hour), false)
	for _, pinned := range []bool{true, false} {
		input, _ := json.Marshal(PreviewPinInput{PreviewTarget: current, Pinned: pinned})
		plan, err := (PreviewPin{Store: store}).Plan(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.Steps[0].Apply(ctx, io.Discard); err != nil {
			t.Fatal(err)
		}
		for _, target := range []PreviewTarget{older, current} {
			rev, err := store.GetRevision(ctx, target.App, target.Revision)
			if err != nil {
				t.Fatal(err)
			}
			var m manifest.Manifest
			if err := json.Unmarshal(rev.Manifest, &m); err != nil {
				t.Fatal(err)
			}
			if m.Preview.Pinned != pinned {
				t.Fatalf("revision %s lost retention setting", target.Revision)
			}
		}
	}
}
