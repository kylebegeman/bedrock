package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/restic"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/state"
)

// coreYAML is shaped like Loom's Core: one image for several workloads, a
// runner built from another target, releases, a database with first-run
// scripts, an object store and made secrets.
const coreYAML = `app: loom
data:
  postgres:
    version: "17"
    image: pgvector/pgvector:pg17
    user: loom_cluster_admin
    database: loom
    init: deploy/postgres
    secrets: [LOOM_CORE_API_PASSWORD]
    database_url: false
  objects: {}
  volumes:
    core-data: {}
secrets:
  generate:
    LOOM_CORE_API_PASSWORD: hex:32
    LOOM_CORE_RUNNER_OPERATOR_TOKEN: hex:32
  derive:
    LOOM_CORE_API_DATABASE_URL: "postgres://loom_core_api:{LOOM_CORE_API_PASSWORD}@{postgres}:5432/loom"
    LOOM_CORE_RUNNER_OPERATOR_TOKEN_SHA256: "sha256:{sha256:LOOM_CORE_RUNNER_OPERATOR_TOKEN}"
    DATABASE_URL: "postgres://loom_core_api:{LOOM_CORE_API_PASSWORD}@{postgres}:5432/loom"
workloads:
  core-api:
    kind: web
    build: {target: server}
    port: 4773
    singleton: true
    secrets: [LOOM_CORE_API_DATABASE_URL, DATABASE_URL]
    mounts: [{volume: core-data, path: /data/loom}]
    routes:
      - host: core.example.com
        path: /api
      - host: runner.example.com
        port: 4774
  product:
    kind: web
    build: {target: server}
    port: 3773
    routes: [{host: core.example.com}]
  runner:
    kind: worker
    build: {target: runner-runtime}
    singleton: true
    privileged: true
    user: "1000"
    resources: {pids: 2048}
    health:
      command: [node, -e, "process.exit(0)"]
  schema:
    kind: release
    order: 2
    build: {target: server}
    command: [node, dist/core-bin.mjs, database, migrate]
  objects-setup:
    kind: release
    order: 1
    image: quay.io/minio/mc:latest
`

func loadCore(t *testing.T) (*manifest.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(coreYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "postgres"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "postgres", "010-roles.sql"), []byte("select 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func newSecrets(t *testing.T) *secrets.Store {
	t.Helper()
	dir := t.TempDir()
	s := &secrets.Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "key")}
	if _, _, _, err := s.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWorkloadsBuiltAlikeShareOneImage(t *testing.T) {
	m, _ := loadCore(t)
	got := buildGroups(m)
	want := [][]string{{"core-api", "product", "schema"}, {"objects-setup"}, {"runner"}}
	if len(got) != len(want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("groups %v, want %v", got, want)
		}
	}
}

func TestTheDeployPlanMakesSecretsBeforeDataAndReleasesBeforeStart(t *testing.T) {
	m, dir := loadCore(t)
	d := Deploy{StateDir: t.TempDir(), Addresses: func(context.Context) []string { return nil }}
	plan, err := d.rollout(context.Background(), m, "20260919-120000", &buildFrom{source: dir})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range plan.Steps {
		names = append(names, s.Name)
	}
	want := []string{"record", "edge", "dns", "secrets", "data", "build-core-api", "pull-objects-setup", "build-runner", "release", "start", "ready", "switch", "certificates", "retire"}
	if !slices.Equal(names, want) {
		t.Fatalf("steps\n %v\nwant\n %v", names, want)
	}
	for _, s := range plan.Steps {
		if s.Name == "release" && !strings.Contains(s.Change, "objects-setup, schema") {
			t.Fatalf("releases run by order, then name: %q", s.Change)
		}
		if s.Name == "build-core-api" && !strings.Contains(s.Change, "core-api, product, schema") {
			t.Fatalf("the shared build names every workload it serves: %q", s.Change)
		}
	}
}

func TestSecretsAreMadeOnceAndDerivedAgainWhenTheirSourceChanges(t *testing.T) {
	m, _ := loadCore(t)
	sec := newSecrets(t)
	var out strings.Builder
	if err := ensureAppSecrets(sec, m, &out); err != nil {
		t.Fatal(err)
	}
	first, v1, err := sec.LoadCurrent("loom")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{postgresPasswordName, ObjectsUserName, ObjectsPasswordName, "LOOM_CORE_API_PASSWORD", "LOOM_CORE_RUNNER_OPERATOR_TOKEN", "LOOM_CORE_API_DATABASE_URL", "LOOM_CORE_RUNNER_OPERATOR_TOKEN_SHA256", "DATABASE_URL"} {
		if first[name] == "" {
			t.Fatalf("%s wasn't made", name)
		}
	}
	if len(first["LOOM_CORE_API_PASSWORD"]) != 64 {
		t.Fatalf("hex:32 makes 64 hex digits: %q", first["LOOM_CORE_API_PASSWORD"])
	}
	wantURL := "postgres://loom_core_api:" + first["LOOM_CORE_API_PASSWORD"] + "@quark-loom-postgres:5432/loom"
	if first["LOOM_CORE_API_DATABASE_URL"] != wantURL {
		t.Fatalf("derived URL %q, want %q", first["LOOM_CORE_API_DATABASE_URL"], wantURL)
	}
	if !strings.HasPrefix(first["LOOM_CORE_RUNNER_OPERATOR_TOKEN_SHA256"], "sha256:") || len(first["LOOM_CORE_RUNNER_OPERATOR_TOKEN_SHA256"]) != 71 {
		t.Fatalf("hash %q", first["LOOM_CORE_RUNNER_OPERATOR_TOKEN_SHA256"])
	}
	// The values never reach the output, only the names.
	for _, v := range first {
		if len(v) > 12 && strings.Contains(out.String(), v) {
			t.Fatalf("a value reached the output: %s", out.String())
		}
	}

	// A second deploy changes nothing.
	out.Reset()
	if err := ensureAppSecrets(sec, m, &out); err != nil {
		t.Fatal(err)
	}
	if _, v2, _ := sec.LoadCurrent("loom"); v2 != v1 {
		t.Fatalf("an unchanged manifest made version %d after %d: %s", v2, v1, out.String())
	}

	// An operator replaces a generated value: it is kept, and what derives
	// from it follows.
	if _, err := sec.Set("loom", "LOOM_CORE_API_PASSWORD", "replaced"); err != nil {
		t.Fatal(err)
	}
	if err := ensureAppSecrets(sec, m, &out); err != nil {
		t.Fatal(err)
	}
	after, _, _ := sec.LoadCurrent("loom")
	if after["LOOM_CORE_API_PASSWORD"] != "replaced" || !strings.Contains(after["LOOM_CORE_API_DATABASE_URL"], ":replaced@") {
		t.Fatalf("derived values must follow their source: %q", after["LOOM_CORE_API_DATABASE_URL"])
	}
	if after[postgresPasswordName] != first[postgresPasswordName] {
		t.Fatal("the database password must never change once made")
	}
}

func TestTheDefaultDatabaseURLNamesTheAppsOwnUser(t *testing.T) {
	m, err := manifest.Parse([]byte("app: shop-front\ndata:\n  postgres: {version: \"16\"}\nworkloads:\n  web:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: shop.example.com}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sec := newSecrets(t)
	if err := ensureAppSecrets(sec, m, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	values, _, _ := sec.LoadCurrent("shop-front")
	want := "postgres://shop_front:" + values[postgresPasswordName] + "@quark-shop-front-postgres:5432/shop_front?sslmode=disable"
	if values[DatabaseURLName] != want {
		t.Fatalf("DATABASE_URL %q, want %q", values[DatabaseURLName], want)
	}
	spec, err := containerSpec(m, "web", m.Workloads["web"], "r1", "img", values)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Env, DatabaseURLName+"="+want) {
		t.Fatalf("every workload gets DATABASE_URL: %v", spec.Env)
	}
}

func TestContainerSpecsCarryNamesLimitsAndOnlyTheirOwnSecrets(t *testing.T) {
	m, _ := loadCore(t)
	values := map[string]string{"LOOM_CORE_API_DATABASE_URL": "api-url", "DATABASE_URL": "backup-url"}
	spec, err := containerSpec(m, "core-api", m.Workloads["core-api"], "r1", "img", values)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.Aliases, []string{"core-api"}) {
		t.Fatalf("aliases %v", spec.Aliases)
	}
	// database_url: false, and the workload names DATABASE_URL itself:
	// its own value, once.
	n := 0
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "DATABASE_URL=") {
			n++
			if kv != "DATABASE_URL=backup-url" {
				t.Fatalf("DATABASE_URL is the workload's own: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Fatalf("DATABASE_URL %d times: %v", n, spec.Env)
	}
	runner, err := containerSpec(m, "runner", m.Workloads["runner"], "r1", "img", values)
	if err != nil {
		t.Fatal(err)
	}
	if runner.PidsLimit != 2048 {
		t.Fatalf("pids %d", runner.PidsLimit)
	}
	if !runner.NoHealthcheck || spec.NoHealthcheck {
		t.Fatalf("the image's HEALTHCHECK is off only where the manifest checks health: runner %v, core-api %v", runner.NoHealthcheck, spec.NoHealthcheck)
	}
	for _, kv := range runner.Env {
		if strings.HasPrefix(kv, "DATABASE_URL=") || strings.HasPrefix(kv, "LOOM_CORE_API_DATABASE_URL=") {
			t.Fatalf("the runner got another workload's secret: %s", kv)
		}
	}
}

func TestRoutesReachTheirOwnPorts(t *testing.T) {
	m, _ := loadCore(t)
	name, w, r, ok := m.RouteFor("runner.example.com", "/api/core/runners/control")
	if !ok || name != "core-api" || servePort(w, &r) != 4774 {
		t.Fatalf("runner host: %s %d %v", name, servePort(w, &r), ok)
	}
	name, w, r, ok = m.RouteFor("core.example.com", "/api/x")
	if !ok || name != "core-api" || servePort(w, &r) != 4773 {
		t.Fatalf("api path: %s %d", name, servePort(w, &r))
	}
	name, w, r, ok = m.RouteFor("core.example.com", "/")
	if !ok || name != "product" || servePort(w, &r) != 3773 {
		t.Fatalf("catch-all: %s %d", name, servePort(w, &r))
	}
}

func TestInitScriptsAreCopiedOnlyFromInsideTheSource(t *testing.T) {
	m, dir := loadCore(t)
	stateDir := t.TempDir()
	got, err := prepareInit(m, dir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != InitDir(stateDir, "loom") {
		t.Fatalf("dir %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(got, "010-roles.sql")); err != nil || string(b) != "select 1;\n" {
		t.Fatalf("copied script: %v %q", err, b)
	}
	// A rollback has no source: it uses the copy the last deploy made.
	if again, err := prepareInit(m, "", stateDir); err != nil || again != got {
		t.Fatalf("rollback: %q %v", again, err)
	}

	// A link out of the source is refused, and so is a linked file inside.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "steal.sql"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "deploy", "postgres")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "deploy", "postgres")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareInit(m, dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside the source") {
		t.Fatalf("a link out of the source must be refused: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "deploy", "postgres")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "deploy", "postgres"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "steal.sql"), filepath.Join(dir, "deploy", "postgres", "010-steal.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareInit(m, dir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "no .sql or .sh files") {
		t.Fatalf("a linked script must not be copied: %v", err)
	}
}

func TestARollbackMakesNoSecretsAndRunsNoReleases(t *testing.T) {
	ctx := context.Background()
	m, _ := loadCore(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, _ := json.Marshal(m)
	images := map[string]string{}
	for _, name := range m.WorkloadNames() {
		images[name] = "img-" + name
	}
	if err := store.SaveRevision(ctx, state.Revision{App: "loom", ID: "20260918-120000", Status: state.RevisionPrevious, Manifest: raw, Images: images, Containers: map[string]string{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d := Deploy{Store: store, StateDir: t.TempDir(), Addresses: func(context.Context) []string { return nil }}
	plan, err := d.rollout(ctx, m, "20260918-120000", nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range plan.Steps {
		names = append(names, s.Name)
	}
	want := []string{"edge", "dns", "data", "start", "ready", "switch", "certificates", "retire"}
	if !slices.Equal(names, want) {
		t.Fatalf("rollback steps\n %v\nwant\n %v", names, want)
	}
}

func TestAFailedContainerIsExplainedByItsErrorNotItsLastBrace(t *testing.T) {
	logs := "[20:17:18.450] ERROR (#2): ~effect/cli/CliError/UserError: \n    at catch (file:///app/dist/bin.mjs:72339:20)\n  [cause]: Error: LOOM_RUNNER_CORE_CONTROL_URL is required.\n      at causePrettyError (file:///x.js:228:13)\n}\n"
	if got := lastWords(logs); got != "[cause]: Error: LOOM_RUNNER_CORE_CONTROL_URL is required." {
		t.Fatalf("got %q", got)
	}
	if got := lastWords("listening on 8000\n}\n"); got != "listening on 8000" {
		t.Fatalf("got %q", got)
	}
	if got := lastWords("\n\n"); got != "it wrote nothing; see quark logs" {
		t.Fatalf("got %q", got)
	}
}

func TestStartPreparationFailureRestartsThePreviousContainer(t *testing.T) {
	for _, removalFails := range []bool{false, true} {
		t.Run(fmt.Sprint("removalFails=", removalFails), func(t *testing.T) {
			ctx := context.Background()
			store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			m := &manifest.Manifest{App: "test", Workloads: map[string]manifest.Workload{
				"a": {Kind: manifest.Worker, Image: "img", User: "0", Singleton: true},
				"z": {Kind: manifest.Worker, Image: "img", User: "0"},
			}}
			old := *m
			old.Workloads = map[string]manifest.Workload{"a": {Kind: manifest.Worker, Image: "img", User: "0"}}
			oldJSON, _ := json.Marshal(old)
			newJSON, _ := json.Marshal(m)
			for _, rev := range []state.Revision{
				{App: "test", ID: "old-rev", Status: state.RevisionActive, Manifest: oldJSON, Containers: map[string]string{"a": "old-container"}},
				{App: "test", ID: "new-rev", Status: state.RevisionFailed, Manifest: newJSON, Images: map[string]string{"a": "img"}},
			} {
				if err := store.SaveRevision(ctx, rev); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			running := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := strings.TrimPrefix(r.URL.Path, "/v1.44")
				calls = append(calls, r.Method+" "+path)
				w.Header().Set("Content-Type", "application/json")
				switch {
				case path == "/_ping":
					w.Header().Set("API-Version", "1.44")
					_, _ = io.WriteString(w, "OK")
				case path == "/containers/old-container/json":
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "old-container", "State": map[string]any{"Running": running}})
				case strings.HasSuffix(path, "/json"):
					w.WriteHeader(404)
					_, _ = io.WriteString(w, `{ "message": "not found" }`)
				case path == "/containers/create":
					w.WriteHeader(201)
					_, _ = io.WriteString(w, `{"Id":"new-container"}`)
				case r.Method == http.MethodDelete && removalFails:
					w.WriteHeader(500)
					_, _ = io.WriteString(w, `{"message":"fixture removal failure"}`)
				default:
					if path == "/containers/old-container/stop" {
						running = false
					}
					if path == "/containers/old-container/start" {
						running = true
					}
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp"+strings.TrimPrefix(server.URL, "http"))
			t.Setenv("DOCKER_API_VERSION", "1.44")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			d := Deploy{Store: store, Secrets: newSecrets(t)}
			plan, err := d.rollout(ctx, m, "new-rev", &buildFrom{})
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range plan.Steps {
				if step.Name == "start" {
					err = step.Apply(ctx, io.Discard)
					if err == nil || !strings.Contains(err.Error(), "no image recorded for z") {
						t.Fatalf("wanted late preparation failure, got %v", err)
					}
				}
			}
			if !slices.Contains(calls, "POST /containers/old-container/stop") {
				t.Fatalf("old workload was not stopped: %v", calls)
			}
			if restarted := slices.Contains(calls, "POST /containers/old-container/start"); restarted == removalFails || running == removalFails {
				t.Fatalf("unsafe recovery (removalFails=%v): %v", removalFails, calls)
			}
		})
	}
}

func TestRestoreRequiresOriginalKeysBeforeMakingAny(t *testing.T) {
	m, _ := loadCore(t)
	sec := newSecrets(t)
	if err := requireRestoreSecrets(sec, m); err == nil || !strings.Contains(err.Error(), "original secrets") {
		t.Fatalf("restore accepted missing keys: %v", err)
	}
	if _, version, _ := sec.LoadCurrent(m.App); version != 0 {
		t.Fatal("guard generated replacement keys")
	}
	if err := ensureAppSecrets(sec, m, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := requireRestoreSecrets(sec, m); err != nil {
		t.Fatal(err)
	}
}

func TestDrillIncludesWorkersButNeverReleaseOrCronJobs(t *testing.T) {
	m, _ := loadCore(t)
	m.Workloads["scheduled"] = manifest.Workload{Kind: manifest.Cron}
	names := newDrillNames(t.TempDir(), m)
	if !slices.Equal(sortedKeys(names.containers), []string{"core-api", "product", "runner"}) {
		t.Fatalf("drill workloads: %v", names.containers)
	}
}

func TestResumedPullKeepsEarlierImagesAndPinnedSecrets(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SaveRevision(ctx, state.Revision{App: "test", ID: "resume", Status: state.RevisionFailed, Images: map[string]string{"built": "digest"}, SecretsVersion: 3, Source: "source-commit"}); err != nil {
		t.Fatal(err)
	}
	if err := saveImageReferences(ctx, store, "test", "resume", []string{"pulled"}, "image"); err != nil {
		t.Fatal(err)
	}
	rev, err := store.GetRevision(ctx, "test", "resume")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Images["built"] != "digest" || rev.Images["pulled"] != "image" || rev.SecretsVersion != 3 || rev.Source != "source-commit" {
		t.Fatalf("lost saved progress: %+v", rev)
	}
}

func TestRestoreKeepsItsSnapshotIdentityAcrossReplanning(t *testing.T) {
	staging := t.TempDir()
	original := &restic.Snapshot{ID: "0123456789abcdef", ShortID: "01234567", Time: time.Now().UTC()}
	if err := writeRestoreSnapshot(staging, original); err != nil {
		t.Fatal(err)
	}
	resumed, err := readRestoreSnapshot(staging)
	if err != nil || resumed.ID != original.ID || !resumed.Time.Equal(original.Time) {
		t.Fatalf("lost pinned snapshot: %+v %v", resumed, err)
	}
	if err := writeRestoreSnapshot(staging, &restic.Snapshot{ID: "latest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readRestoreSnapshot(staging); err == nil {
		t.Fatal("accepted a moving snapshot target")
	}
}

func TestRestoreResumeAdoptsItsRunAndUsesTheFetchedSnapshot(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stateDir := t.TempDir()
	staging := filepath.Join(stateDir, "restore", "test")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	mf, _ := json.Marshal(manifest.Manifest{App: "test", Workloads: map[string]manifest.Workload{"worker": {Kind: manifest.Worker, Image: "fixture"}}})
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), mf, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := &restic.Snapshot{ID: "0123456789abcdef", ShortID: "01234567", Time: time.Now().UTC()}
	if err := writeRestoreSnapshot(staging, snapshot); err != nil {
		t.Fatal(err)
	}
	id, err := store.StartBackupRun(ctx, "test", state.BackupRunRestore, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	r := RestoreDef{Store: store, Secrets: newSecrets(t), StateDir: stateDir}
	plan, err := r.Plan(ctx, json.RawMessage(`{"app":"test","snapshot":"latest"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The kernel skips the completed fetch when it replans after a crash.
	for _, step := range plan.Steps {
		if step.Name == "fetch" {
			continue
		}
		if err := step.Apply(ctx, io.Discard); err != nil {
			t.Fatalf("%s: %v", step.Name, err)
		}
	}
	run, err := store.LastBackupRun(ctx, "test", state.BackupRunRestore)
	if err != nil || run == nil {
		t.Fatalf("missing recovery record: %v", err)
	}
	if run.ID != id || !run.OK || run.Snapshot != snapshot.ShortID || !run.Finished() {
		t.Fatalf("incorrect recovery receipt: %+v", run)
	}
}
