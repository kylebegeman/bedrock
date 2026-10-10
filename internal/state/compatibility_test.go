package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewerStateIsRejectedBeforeFilesOrSchemaChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE future_records(value TEXT); INSERT INTO future_records VALUES('keep'); PRAGMA user_version=99;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	entriesBefore, _ := os.ReadDir(filepath.Dir(path))
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future schema accepted: %v", err)
	}
	after, _ := os.ReadFile(path)
	entriesAfter, _ := os.ReadDir(filepath.Dir(path))
	if string(before) != string(after) || !reflect.DeepEqual(entryNames(entriesBefore), entryNames(entriesAfter)) {
		t.Fatal("incompatible open changed state files")
	}
}
func entryNames(entries []os.DirEntry) []string {
	out := []string{}
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func TestHistoricalMigrationPreservesJournalAndAddsBothShippedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE revisions(app TEXT NOT NULL,id TEXT NOT NULL,status TEXT NOT NULL,manifest TEXT NOT NULL,images TEXT NOT NULL,containers TEXT NOT NULL,source TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,PRIMARY KEY(app,id));
 INSERT INTO revisions VALUES('site','r1','active','{}','{}','{}','source',123);
 CREATE TABLE job_runs(id INTEGER PRIMARY KEY AUTOINCREMENT,app TEXT NOT NULL,workload TEXT NOT NULL,revision TEXT NOT NULL,kind TEXT NOT NULL,started_at INTEGER NOT NULL,finished_at INTEGER NOT NULL DEFAULT 0,exit_code INTEGER NOT NULL DEFAULT -1,error TEXT NOT NULL DEFAULT '',output TEXT NOT NULL DEFAULT '');
 INSERT INTO job_runs(app,workload,revision,kind,started_at,output) VALUES('site','cron','r1','cron',124,'keep output');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	v, err := store.Version(context.Background())
	if err != nil || v != SchemaVersion {
		t.Fatalf("%d %v", v, err)
	}
	var output, container string
	if err := store.db.QueryRow(`SELECT output,container FROM job_runs`).Scan(&output, &container); err != nil || output != "keep output" || container != "" {
		t.Fatalf("%s %s %v", output, container, err)
	}
	revision, err := store.GetRevision(context.Background(), "site", "r1")
	if err != nil || revision.SecretsVersion != 0 || revision.Source != "source" {
		t.Fatalf("%+v %v", revision, err)
	}
}

func TestFailedMigrationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, _ := sql.Open("sqlite", path)
	_, err := db.Exec(`CREATE TABLE job_runs(id INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("malformed historical schema migrated")
	}
	db, _ = sql.Open("sqlite", path)
	defer db.Close()
	var version, count int
	db.QueryRow(`PRAGMA user_version`).Scan(&version)
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='operations'`).Scan(&count)
	if version != 0 || count != 0 {
		t.Fatalf("partial migration persisted: version %d operations %d", version, count)
	}
}
func TestReadOnlyStateAndCheckpointRetainSource(t *testing.T) {
	source := open(t)
	ctx := context.Background()
	source.AddWatch(ctx, "https://site.example.com", fromUnix(123))
	path := filepath.Join(t.TempDir(), "private", "before.db")
	if err := source.Checkpoint(ctx, path); err != nil {
		t.Fatal(err)
	}
	source.AddWatch(ctx, "https://later.example.com", fromUnix(124))
	if err := source.Checkpoint(ctx, path); err != nil {
		t.Fatal(err)
	}
	saved, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer saved.Close()
	watches, _ := saved.Watches(ctx)
	if len(watches) != 1 {
		t.Fatalf("checkpoint replaced during retry: %+v", watches)
	}
	if err := saved.AddWatch(ctx, "https://write.example.com", fromUnix(125)); err == nil {
		t.Fatal("read-only store wrote")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("checkpoint mode: %v", info.Mode())
	}
	missing := filepath.Join(t.TempDir(), "absent", "state.db")
	v, err := InspectVersion(missing)
	if err != nil || v != nil {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("version inspection created state directory")
	}
}

func TestLegacyMigrationPreservesOperationJournalAndSavedOutput(t *testing.T) {
	ctx := context.Background()
	legacy := open(t)
	path := legacy.Path()
	now := time.Now()
	if err := legacy.CreateOperation(ctx, NewOperation{ID: "old-op", Kind: "old.kind", Target: "site", Input: json.RawMessage(`{}`), Plan: json.RawMessage(`{}`), PlanDigest: "stable", Recovery: "resume", StepNames: []string{"first"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.StepFinished(ctx, "old-op", 0, StepSucceeded, "", "saved historical output", now); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.db.ExecContext(ctx, "PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	op, steps, err := migrated.Get(ctx, "old-op")
	if err != nil || op.PlanDigest != "stable" || len(steps) != 1 || steps[0].Output != "saved historical output" {
		t.Fatalf("%+v %+v %v", op, steps, err)
	}
}
