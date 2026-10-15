package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

func beginMoveReleaseRestore(t *testing.T, store *state.Store, app string) {
	t.Helper()
	ctx := context.Background()
	b := state.ReleaseBoundary{App: app, Revision: "pending-release", Compatibility: manifest.DataRestoreRequired, Snapshot: strings.Repeat("a", 64), StartedAt: time.Now().UTC()}
	if err := store.BeginReleaseBoundary(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginReleaseRestore(ctx, b.App, b.Revision, b.Snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestMoveAndOrdinaryRestoreCannotBypassPendingReleaseRestore(t *testing.T) {
	for _, kind := range []string{"capture", "stage", "test", "cutover", "resume", "restore"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store, m, h, source := safeMoveFixture(t)
			deploy := Deploy{Store: store, Secrets: newSecrets(t), StateDir: filepath.Dir(store.Path())}
			var def kernel.Definition
			var input any
			switch kind {
			case "capture":
				data, _ := json.Marshal(m)
				if err := store.SaveRevision(ctx, state.Revision{App: m.App, ID: h.Revision, Status: state.RevisionActive, Manifest: data, CreatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
				def = MoveCapture{Backup: Backup{Store: store, Secrets: deploy.Secrets, StateDir: deploy.StateDir}}
				input = MoveCaptureInput{App: m.App, From: h.From, To: h.To, Revision: h.Revision, Final: true}
			case "stage":
				def = MoveStage{Deploy: deploy, Hostname: func() string { return h.To }}
				input = MoveStageInput{Source: source, Revision: MoveTargetRevision(h), Handoff: h}
			case "test":
				h.Phase = "rehearsal"
				def = MoveTest{Deploy: deploy, Hostname: func() string { return h.To }}
				input = MoveStageInput{Source: source, Revision: MoveTargetRevision(h), Handoff: h}
			case "cutover":
				revision := MoveTargetRevision(h)
				data, _ := json.Marshal(m)
				if err := store.SaveRevision(ctx, state.Revision{App: m.App, ID: revision, Status: revisionStaged, Manifest: data, CreatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
				if err := saveMigration(store.Path(), &migrationRecord{Handoff: h, Role: "target", Phase: "staged", TargetRevision: revision}); err != nil {
					t.Fatal(err)
				}
				def = MoveCutover{Deploy: deploy}
				input = MoveCutoverInput{App: m.App, Migration: h.Migration, Revision: revision}
			case "resume":
				if err := saveMigration(store.Path(), &migrationRecord{Handoff: h, Role: "source", Phase: "fenced"}); err != nil {
					t.Fatal(err)
				}
				def = MoveResume{Store: store}
				input = MoveResumeInput{App: m.App, Migration: h.Migration, TargetStopped: true}
			case "restore":
				def = RestoreDef{Store: store, Secrets: deploy.Secrets, StateDir: deploy.StateDir}
				input = RestoreInput{App: m.App, Snapshot: strings.Repeat("b", 64)}
			}
			raw, _ := json.Marshal(input)
			plan, err := def.Plan(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			beginMoveReleaseRestore(t, store, m.App)
			if _, err := def.Plan(ctx, raw); !errors.Is(err, ErrReleaseRestorePending) {
				t.Fatalf("%s plan bypassed a pending checkpoint restore: %v", kind, err)
			}
			for _, step := range plan.Steps {
				if err := step.Apply(ctx, io.Discard); !errors.Is(err, ErrReleaseRestorePending) {
					t.Fatalf("reviewed %s/%s bypassed a later checkpoint restore: %v", kind, step.Name, err)
				}
			}
		})
	}
}

func TestMoveRehearsalCanCompensateDuringPendingReleaseRestore(t *testing.T) {
	ctx := context.Background()
	dockertest.New(t)
	store, m, h, source := safeMoveFixture(t)
	h.Phase = "rehearsal"
	def := MoveTest{Deploy: Deploy{Store: store, Secrets: newSecrets(t), StateDir: filepath.Dir(store.Path())}, Hostname: func() string { return h.To }}
	in := MoveStageInput{Source: source, Revision: MoveTargetRevision(h), Handoff: h}
	raw, _ := json.Marshal(in)
	plan, err := def.Plan(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps[:2] {
		if err := step.Apply(ctx, io.Discard); err != nil {
			t.Fatalf("%s: %v", step.Name, err)
		}
	}
	beginMoveReleaseRestore(t, store, m.App)
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	recovery, err := def.Plan(ctx, raw)
	if err != nil {
		t.Fatalf("pending checkpoint prevented rehearsal compensation planning: %v", err)
	}
	if err := recovery.Steps[0].Undo(ctx, io.Discard); err != nil {
		t.Fatalf("pending checkpoint prevented scratch cleanup: %v", err)
	}
	if _, err := store.GetRevision(ctx, m.App, in.Revision); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("scratch revision survived compensation: %v", err)
	}
	if err := checkReleaseStoreAccess(ctx, store, m.App); !errors.Is(err, ErrReleaseRestorePending) {
		t.Fatalf("scratch cleanup cleared the checkpoint fence: %v", err)
	}
}
