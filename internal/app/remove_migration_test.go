package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/state"
)

func TestRemoveClearsMigrationMetadataAfterContainers(t *testing.T) {
	for _, role := range []string{"source", "target", "leftovers"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			fake := dockertest.New(t)
			store, m, h, _ := safeMoveFixture(t)
			stateDir := filepath.Dir(store.Path())
			if role != "leftovers" {
				status := state.RevisionActive
				if role == "target" {
					status = revisionStaged
				}
				data, _ := json.Marshal(m)
				if err := store.SaveRevision(ctx, state.Revision{App: m.App, ID: h.Revision, Status: status, Manifest: data, CreatedAt: time.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			addMoveWriter(fake, "bedrock-site-web-r1", "web", true)
			fenceRole, phase := role, "fenced"
			if role == "leftovers" {
				fenceRole = "source"
			} else if role == "target" {
				phase = "staged"
			}
			if err := saveMigration(store.Path(), &migrationRecord{Handoff: h, Role: fenceRole, Phase: phase, TargetRevision: h.Revision}); err != nil {
				t.Fatal(err)
			}
			captureDir := filepath.Join(stateDir, "moves", m.App, "capture")
			if err := os.MkdirAll(captureDir, 0o700); err != nil {
				t.Fatal(err)
			}
			remove := Remove{Store: store, Secrets: newSecrets(t), StateDir: stateDir}
			raw, _ := json.Marshal(RemoveInput{App: m.App, Data: role == "leftovers"})
			plan, err := remove.Plan(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range plan.Steps {
				if step.Name == "containers" {
					if err := CheckMigrationAccess(store.Path(), m.App); err == nil {
						t.Fatal("fence cleared before removing writers")
					}
				}
				if err := step.Apply(ctx, io.Discard); err != nil {
					t.Fatalf("%s: %v", step.Name, err)
				}
			}
			if _, exists := fake.Container("bedrock-site-web-r1"); exists {
				t.Fatal("app writer survived explicit removal")
			}
			if err := CheckMigrationAccess(store.Path(), m.App); err != nil {
				t.Fatalf("removed app cannot reuse its name: %v", err)
			}
			for _, path := range []string{migrationFile(store.Path(), m.App), filepath.Dir(captureDir)} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("migration metadata survived at %s: %v", path, err)
				}
			}
		})
	}
}

func TestRemoveFindsAnOrphanMigrationFenceWithoutAppRows(t *testing.T) {
	dockertest.New(t)
	store, _, h, _ := safeMoveFixture(t)
	if err := saveMigration(store.Path(), &migrationRecord{Handoff: h, Role: "source", Phase: "fenced"}); err != nil {
		t.Fatal(err)
	}
	r := Remove{Store: store, StateDir: filepath.Dir(store.Path())}
	plan, err := r.Plan(context.Background(), json.RawMessage(`{"app":"site","data":true}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if err := step.Apply(context.Background(), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckMigrationAccess(store.Path(), h.App); err != nil {
		t.Fatal(err)
	}
}
