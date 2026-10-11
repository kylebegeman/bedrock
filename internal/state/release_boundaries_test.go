package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseCheckpointAndUncertainBoundarySurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	original := ReleaseBoundary{App: "site", Revision: "candidate", PreviousRevision: "old", Compatibility: "unknown", Snapshot: strings.Repeat("a", 64), StartedAt: now}
	if err := store.PrepareReleaseBoundary(ctx, original); err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.Snapshot = strings.Repeat("b", 64)
	if err := store.PrepareReleaseBoundary(ctx, changed); err != nil {
		t.Fatal(err)
	}
	started, err := store.StartPreparedReleaseBoundary(ctx, "site", "candidate", now.Add(time.Second))
	if err != nil || !started {
		t.Fatalf("%v %v", started, err)
	}
	started, err = store.StartPreparedReleaseBoundary(ctx, "site", "candidate", now.Add(time.Second))
	if err != nil || started {
		t.Fatalf("mutation replay admitted: %v %v", started, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.BeginReleaseBoundary(ctx, changed); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.ReleaseBoundary(ctx, "site", "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Status != ReleaseStarted || boundary.Snapshot != original.Snapshot || boundary.PreviousRevision != "old" {
		t.Fatalf("uncertain boundary overwritten: %+v", boundary)
	}
	if err := store.FinishReleaseBoundary(ctx, "site", "candidate", ReleaseFailed, "mutation failed after writes", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	boundaries, err := store.ReleaseBoundaries(ctx, "site")
	if err != nil || len(boundaries) != 1 || boundaries[0].Status != ReleaseFailed || boundaries[0].Error == "" {
		t.Fatalf("%+v %v", boundaries, err)
	}
}

func TestReleaseRestoreFenceAndCompletionAreAtomicAndKeepHistory(t *testing.T) {
	ctx := context.Background()
	store := open(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, rev := range []Revision{{App: "site", ID: "old", Status: RevisionPrevious, Manifest: json.RawMessage(`{"app":"site"}`), SecretsVersion: 4, CreatedAt: now}, {App: "site", ID: "new", Status: RevisionActive, CreatedAt: now.Add(time.Minute)}, {App: "other", ID: "unrelated", Status: RevisionActive, CreatedAt: now}} {
		if err := store.SaveRevision(ctx, rev); err != nil {
			t.Fatal(err)
		}
	}
	store.Activate(ctx, "site", "new", now)
	store.Activate(ctx, "other", "unrelated", now)
	anchor := ReleaseBoundary{App: "site", Revision: "new", PreviousRevision: "old", Compatibility: "restore-required", Snapshot: strings.Repeat("a", 64), StartedAt: now.Add(time.Minute)}
	later := anchor
	later.Revision = "failed-later"
	later.StartedAt = now.Add(2 * time.Minute)
	for _, b := range []ReleaseBoundary{anchor, later} {
		if err := store.BeginReleaseBoundary(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishReleaseBoundary(ctx, b.App, b.Revision, ReleaseFailed, "unsafe", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.BeginReleaseRestore(ctx, anchor.App, anchor.Revision, strings.Repeat("b", 64)); err == nil {
		t.Fatal("mismatched checkpoint admitted")
	}
	current, _ := store.RevisionWithStatus(ctx, "site", RevisionActive)
	if current == nil {
		t.Fatal("failed fence changed authority")
	}
	if err := store.BeginReleaseRestore(ctx, anchor.App, anchor.Revision, anchor.Snapshot); err != nil {
		t.Fatal(err)
	}
	current, _ = store.RevisionWithStatus(ctx, "site", RevisionActive)
	if current != nil {
		t.Fatal("fenced app still advertised active")
	}
	boundaries, _ := store.ReleaseBoundaries(ctx, "site")
	if boundaries[1].Status != ReleaseRestoring {
		t.Fatalf("missing fence: %+v", boundaries)
	}
	if err := store.CompleteReleaseRestore(ctx, anchor, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	boundaries, _ = store.ReleaseBoundaries(ctx, "site")
	for _, b := range boundaries {
		if b.Status != ReleaseRestored {
			t.Fatalf("unrestored boundary: %+v", b)
		}
	}
	old, err := store.GetRevision(ctx, "site", "old")
	if err != nil || old.SecretsVersion != 4 {
		t.Fatalf("original metadata lost: %+v %v", old, err)
	}
	other, _ := store.RevisionWithStatus(ctx, "other", RevisionActive)
	if other == nil {
		t.Fatal("unrelated app changed")
	}
}

func TestRemoveAppClearsReleaseFencesAtomicallyAndPermitsSameNameReuse(t *testing.T) {
	ctx := context.Background()
	store := open(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, app := range []string{"site", "other"} {
		if err := store.SaveRevision(ctx, Revision{App: app, ID: "candidate", Status: RevisionActive, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(ctx, app, "candidate", at); err != nil {
			t.Fatal(err)
		}
		boundary := ReleaseBoundary{App: app, Revision: "candidate", PreviousRevision: "old-code", Compatibility: "restore-required", Snapshot: strings.Repeat("a", 64), StartedAt: at}
		if err := store.BeginReleaseBoundary(ctx, boundary); err != nil {
			t.Fatal(err)
		}
		if err := store.BeginReleaseRestore(ctx, app, boundary.Revision, boundary.Snapshot); err != nil {
			t.Fatal(err)
		}
	}
	// A failed boundary cleanup must retain the revision history needed to
	// retry removal or recover, rather than strand a fence with no target.
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER refuse_boundary_cleanup BEFORE DELETE ON release_boundaries WHEN OLD.app='site' BEGIN SELECT RAISE(ABORT, 'fixture cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveApp(ctx, "site"); err == nil {
		t.Fatal("release-fence cleanup failure did not fail removal")
	}
	if _, err := store.GetRevision(ctx, "site", "candidate"); err != nil {
		t.Fatalf("failed removal lost retryable revision history: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER refuse_boundary_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveApp(ctx, "site"); err != nil {
		t.Fatal(err)
	}
	if boundaries, err := store.ReleaseBoundaries(ctx, "site"); err != nil || len(boundaries) != 0 {
		t.Fatalf("removed app left an unresolvable release fence: %+v, %v", boundaries, err)
	}
	if boundaries, err := store.ReleaseBoundaries(ctx, "other"); err != nil || len(boundaries) != 1 || boundaries[0].Status != ReleaseRestoring {
		t.Fatalf("removal changed another app's release fence: %+v, %v", boundaries, err)
	}
	fresh := ReleaseBoundary{App: "site", Revision: "candidate", Compatibility: "unknown", Snapshot: strings.Repeat("b", 64), StartedAt: at.Add(time.Minute)}
	if err := store.PrepareReleaseBoundary(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.ReleaseBoundary(ctx, fresh.App, fresh.Revision)
	if err != nil || boundary.Status != ReleasePrepared || boundary.Snapshot != fresh.Snapshot {
		t.Fatalf("same-name app inherited obsolete checkpoint state: %+v, %v", boundary, err)
	}
}
