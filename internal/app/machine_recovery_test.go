package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/restic"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/kylebegeman/bedrock/internal/version"
)

func recoveryFixture(t *testing.T) (RecoverMachine, *secrets.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sec := &secrets.Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "target.key")}
	if _, _, _, err := sec.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := integration.Set(sec, integration.StorageName, map[string]string{"kind": "s3", "endpoint": "https://storage.example.test", "key_id": "target-id", "key": "target-storage-value", "bucket_prefix": "target", "password": "same-backup-password"}); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	source := &secrets.Store{Dir: filepath.Join(sourceDir, "secrets"), KeyPath: filepath.Join(sourceDir, "source.key")}
	if _, _, _, err := source.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	return RecoverMachine{Store: st, Secrets: sec, StateDir: dir}, source
}

func recoveryInput(rehearsal bool) RecoverMachineInput {
	in := RecoverMachineInput{Source: "old-host", Snapshot: strings.Repeat("a", 64), Rehearsal: rehearsal}
	in.ID = RecoveryID(in.Source, in.Snapshot, rehearsal)
	return in
}

func recoveryManifest() manifest.Manifest {
	return manifest.Manifest{App: "hello", Workloads: map[string]manifest.Workload{
		"web": {Kind: manifest.Web, Singleton: true, Image: "nginx:alpine", Port: 8080, Ports: []manifest.PublishedPort{{Port: 8080, HostPort: 8080}}, Routes: []manifest.Route{{Host: "hello.example.test"}}},
	}}
}

func TestRecoveryPinsExplicitUnambiguousSnapshots(t *testing.T) {
	a := restic.Snapshot{ID: "deadbeef" + strings.Repeat("a", 56)}
	b := restic.Snapshot{ID: "deadbeef" + strings.Repeat("b", 56)}
	if _, err := resolveRecoverySnapshot([]restic.Snapshot{a, b}, "deadbeef"); err == nil {
		t.Fatal("an ambiguous prefix must refuse")
	}
	if _, err := resolveRecoverySnapshot([]restic.Snapshot{a}, "latest"); err == nil {
		t.Fatal("latest cannot be a recovery identity")
	}
	snap, err := resolveRecoverySnapshot([]restic.Snapshot{a, b}, a.ID)
	if err != nil || snap.ID != a.ID {
		t.Fatalf("exact selection failed: %v", err)
	}
	if _, err := resolveRecoverySnapshot([]restic.Snapshot{a}, strings.Repeat("c", 64)); err == nil {
		t.Fatal("a missing pinned snapshot cannot fall back to a newer backup")
	}
}

func TestRecoveryRejectsIncompatibleBuildsAndImagePlatforms(t *testing.T) {
	target := version.Info{Version: "0.7.0-dev", OS: "linux", Arch: "arm64"}
	base := machineRecoveryCatalog{Format: machineRecoveryFormat, Version: "0.7.0", OS: "linux", Arch: "arm64"}
	if err := checkMachineRecoveryCompatibility(base, target); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]machineRecoveryCatalog{
		"future-version": {Format: machineRecoveryFormat, Version: "0.8.0", OS: "linux", Arch: "arm64"},
		"other-arch":     {Format: machineRecoveryFormat, Version: "0.7.0", OS: "linux", Arch: "amd64"},
		"unknown-format": {Format: 99, Version: "0.7.0", OS: "linux", Arch: "arm64"},
	} {
		t.Run(name, func(t *testing.T) {
			if checkMachineRecoveryCompatibility(c, target) == nil {
				t.Fatal("unsupported backup must fail before resources are created")
			}
		})
	}
}

func TestRecoverySourceIdentityIsSealedBoundAndReused(t *testing.T) {
	r, source := recoveryFixture(t)
	in := recoveryInput(false)
	raw, err := os.ReadFile(source.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore, _ := os.ReadFile(r.Secrets.KeyPath)
	hash, err := SealRecoveryIdentity(r.Secrets, r.StateDir, in.ID, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := os.ReadFile(filepath.Join(RecoveryDirectory(r.StateDir, in.ID), "identity.age"))
	if bytes.Contains(cipher, bytes.TrimSpace(raw)) {
		t.Fatal("source identity reached unsealed disk")
	}
	if _, err := openRecoveryIdentity(r.Secrets, r.StateDir, in.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("checksum mismatch must refuse")
	}
	opened, err := openRecoveryIdentity(r.Secrets, r.StateDir, in.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	want, err := parseRecoveryIdentity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Recipient().String() != want.Recipient().String() {
		t.Fatal("sealed identity opened the wrong source")
	}
	second, err := SealRecoveryIdentity(r.Secrets, r.StateDir, in.ID, bytes.NewReader(raw))
	if err != nil || second != hash {
		t.Fatal("a retry must keep the original protected artifact and input digest")
	}
	other, _, _, err := r.Secrets.EnsureKey()
	if err != nil || other == "" {
		t.Fatal("target identity disappeared")
	}
	targetAfter, _ := os.ReadFile(r.Secrets.KeyPath)
	if !bytes.Equal(targetBefore, targetAfter) {
		t.Fatal("recovery replaced the target private identity")
	}
	invalid := []byte("AGE-SECRET-KEY-invalid-sensitive-input")
	if _, err := SealRecoveryIdentity(r.Secrets, r.StateDir, "recover-invalid", bytes.NewReader(invalid)); err == nil || strings.Contains(err.Error(), "invalid-sensitive-input") {
		t.Fatal("invalid keys must be rejected without echoing input")
	}
}

func TestRecoveryResealedRetryReusesDigestWithoutRecreatingIdentity(t *testing.T) {
	r, source := recoveryFixture(t)
	in := recoveryInput(true)
	raw, err := os.ReadFile(source.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	in.IdentitySHA256, err = SealRecoveryIdentity(r.Secrets, r.StateDir, in.ID, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := targetRecoveryIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	rec := &machineRecoveryRecord{Input: in, SecretsReady: true, FinishedAt: time.Now(), Catalog: machineRecoveryCatalog{Recipient: identity.Recipient().String()}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(x.dir, "identity.age")
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	hash, err := SealRecoveryIdentity(r.Secrets, r.StateDir, in.ID, bytes.NewReader(raw))
	if err != nil || hash != in.IdentitySHA256 {
		t.Fatal("completed retry changed the originally journaled identity digest:", err)
	}
	if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed retry recreated a source identity that is no longer needed")
	}
	other, err := os.ReadFile(r.Secrets.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SealRecoveryIdentity(r.Secrets, r.StateDir, in.ID, bytes.NewReader(other)); err == nil {
		t.Fatal("a resealed retry accepted a different source identity")
	}
}

func TestRecoveryResealsEveryVersionAndInstallsIdempotently(t *testing.T) {
	r, source := recoveryFixture(t)
	if _, err := source.Set("hello", "API_TOKEN", "original-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Set("hello", "API_TOKEN", "rotated-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Set("bedrock", "CLOUDFLARE_TOKEN", "source-cloudflare"); err != nil {
		t.Fatal(err)
	}
	sourceID, _ := targetRecoveryIdentity(source)
	targetID, _ := targetRecoveryIdentity(r.Secrets)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	if err := os.MkdirAll(x.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mapping, err := resealRecoverySecrets(source.Dir, x.stagedSecrets().Dir, sourceID, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mapping["hello"], map[int]int{1: 1, 2: 2}) {
		t.Fatal("source version identities were not preserved")
	}
	if got, err := x.stagedSecrets().Load("hello", 1); err != nil || got["API_TOKEN"] != "original-token" {
		t.Fatal("historical secrets are not readable with target identity")
	}
	if got, err := x.stagedSecrets().Load("hello", 2); err != nil || got["API_TOKEN"] != "rotated-token" {
		t.Fatal("current secrets are not readable with target identity")
	}
	before, _, _ := r.Secrets.LoadCurrent("bedrock")
	rec := &machineRecoveryRecord{SecretVersions: mapping}
	if err := x.installAppSecrets(rec); err != nil {
		t.Fatal(err)
	}
	if err := x.installAppSecrets(rec); err != nil {
		t.Fatal("retry must adopt an identical completed secret history:", err)
	}
	current, _ := r.Secrets.Current("hello")
	if current != 2 {
		t.Fatal("recovery renumbered the current secret version")
	}
	after, _, _ := r.Secrets.LoadCurrent("bedrock")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("app recovery replaced target integration credentials")
	}
	if _, err := r.Secrets.Set("hello", "ADDED_AFTER_RECOVERY", "new-value"); err != nil {
		t.Fatal(err)
	}
	if err := x.installAppSecrets(rec); err == nil {
		t.Fatal("a target's changed secret history must refuse instead of being overwritten")
	}
	wrong := &secrets.Store{Dir: x.stagedSecrets().Dir, KeyPath: source.KeyPath}
	if _, err := wrong.Load("hello", 1); err == nil {
		t.Fatal("resealed app values must no longer be readable with the source key")
	}
}

func TestRecoveryRehearsalLeavesTargetAppsAndProfileOutsideItsPlan(t *testing.T) {
	r, _ := recoveryFixture(t)
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	if err := r.Store.SaveRevision(context.Background(), state.Revision{App: m.App, ID: "existing", Status: state.RevisionActive, Manifest: body, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	in := recoveryInput(false)
	raw, _ := json.Marshal(in)
	if _, err := r.Plan(context.Background(), raw); err == nil {
		t.Fatal("real recovery must refuse target app history")
	}
	in = recoveryInput(true)
	raw, _ = json.Marshal(in)
	plan, err := r.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if step.Name == "profile" || step.Name == "state" {
			t.Fatal("rehearsal must not install source profile, credentials or operational state")
		}
	}
}

func TestRecoveryPrivateResourcesCannotOverlapOrPublishLiveWorkloads(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	n := x.names(&m)
	if n.network == docker.AppNetwork(m.App) || n.containers["web"] == docker.ContainerName(m.App, "web", "existing") {
		t.Fatal("rehearsal names collide with live resources")
	}
	spec, err := drillSpec(&m, "recovered-test", "web", "sha256:"+strings.Repeat("b", 64), map[string]string{}, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Publish) > 0 || spec.HostNetwork || spec.Restart || !reflect.DeepEqual(spec.Networks, []string{n.network}) {
		t.Fatal("private recovery workload can reach live networks or published ports")
	}
	second := *x
	second.in.ID = "rehearse-another-test"
	if second.names(&m).network == n.network {
		t.Fatal("independent rehearsals share scratch resources")
	}
}

func TestRecoveryFailedClaimNeverDeletesAnExistingContainer(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, Fetched: true, Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body}}}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	s := dockertest.New(t)
	name := x.names(&m).containers["web"]
	s.Add(dockertest.Container{Name: name, Running: true})
	step := x.step("claim", "claim resources", func(ctx context.Context, out io.Writer) error {
		e, err := docker.Connect(ctx)
		if err != nil {
			return err
		}
		defer e.Close()
		return x.claimResources(ctx, rec, e)
	})
	if err := step.Apply(context.Background(), io.Discard); err == nil {
		t.Fatal("existing resource must refuse")
	}
	if _, ok := s.Container(name); !ok {
		t.Fatal("failed claim cleanup removed a container the recovery did not own")
	}
}

func TestRecoveryFailedClaimPreservesAnExistingPrivateNetwork(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, Fetched: true, Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body}}}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	s := dockertest.New(t)
	name := x.names(&m).network
	s.AddNetwork(name, nil)
	step := x.step("claim", "claim resources", func(ctx context.Context, out io.Writer) error {
		e, err := docker.Connect(ctx)
		if err != nil {
			return err
		}
		defer e.Close()
		return x.claimResources(ctx, rec, e)
	})
	if err := step.Apply(context.Background(), io.Discard); err == nil {
		t.Fatal("existing private network must refuse before resource ownership is claimed")
	}
	if got := s.NetworkNames(); len(got) != 1 || got[0] != name {
		t.Fatal("failed claim cleanup removed a network the recovery did not own")
	}
}

func TestRecoveryCompletedRehearsalResumeDoesNotRecreateResources(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	if err := x.save(&machineRecoveryRecord{Input: in, FinishedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	called := false
	step := x.step("ready", "prove private readiness", func(context.Context, io.Writer) error { called = true; return errors.New("must not run") })
	if err := step.Apply(context.Background(), io.Discard); err != nil || called {
		t.Fatal("completed rehearsal retry recreated disposable resources")
	}
}

func TestRecoveryCutoverRequiresCompletedRealRecoveryAndEveryAppReady(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, StateInstalled: true, FinishedAt: time.Now(), Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body}}}, Ready: map[string]bool{m.App: true}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	c := RecoverCutover{Recovery: r}
	raw, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	if _, err := c.Plan(context.Background(), raw); err == nil {
		t.Fatal("a rehearsal must never be promotable to live traffic")
	}
	rec.Input.Rehearsal = false
	x.in.Rehearsal = false
	rec.Ready[m.App] = false
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Plan(context.Background(), raw); err == nil {
		t.Fatal("a missing private readiness gate must refuse cutover")
	}
}

func TestRecoveryDataLayoutMismatchFailsBeforeWorkloadsStart(t *testing.T) {
	current := recoveryManifest()
	current.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"uploads": {}}}
	backed := recoveryManifest()
	backed.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"different": {}}}
	if compatibleRecoveryData(&current, &backed) == nil {
		t.Fatal("old backup volume layout cannot silently match a newer recovered manifest")
	}
}

func TestRecoveryEmbeddedDataDoesNotDependOnRetainedAppRepositories(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	m.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"uploads": {}}}
	body, _ := json.Marshal(m)
	rev := state.Revision{App: m.App, ID: "source-current", Manifest: body, CreatedAt: time.Now().Add(-time.Hour)}
	provenance := newAppBackupProvenance(rev, time.Now())
	rec := &machineRecoveryRecord{Input: in, Snapshot: restic.Snapshot{ID: in.Snapshot}, Catalog: machineRecoveryCatalog{EmbeddedData: true, Revisions: []state.Revision{rev}, AppSnapshots: map[string]string{m.App: strings.Repeat("c", 64)}, AppData: map[string]appBackupProvenance{m.App: provenance}}, DataReady: map[string]bool{}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	s := dockertest.New(t)
	var restored dockertest.Container
	s.OnCreate = func(c dockertest.Container) {
		if len(c.Cmd) > 0 && c.Cmd[0] == "restore" {
			restored = c
			if err := os.WriteFile(filepath.Join(x.names(&m).dir, "manifest.json"), body, 0o600); err != nil {
				t.Error(err)
			}
			if err := writeRecoveryJSON(filepath.Join(x.names(&m).dir, appBackupProvenanceFile), provenance); err != nil {
				t.Error(err)
			}
			s.Update(c.Name, func(c *dockertest.Container) {
				c.ExitAfter = 1
				c.Logs = `{"message_type":"summary","files_restored":1,"total_files":1}`
			})
		}
	}
	if err := x.data(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(restored.Cmd) < 2 || restored.Cmd[1] != in.Snapshot {
		t.Fatal("embedded data restore followed an external app snapshot instead of the exact machine snapshot")
	}
	wantMount := x.names(&m).volumes["uploads"] + ":" + restic.DataRoot + "/" + machineAppDataDir + "/hello/volumes/uploads"
	found := false
	for _, bind := range restored.HostConfig.Binds {
		if bind == wantMount {
			found = true
		}
	}
	if !found {
		t.Fatal("machine snapshot files were not mapped directly into the isolated app volume")
	}
	if !strings.Contains(strings.Join(restored.Env, "\n"), "target-machine-old-host") {
		t.Fatal("embedded recovery read the app repository, which retention may have removed")
	}
	if err := x.data(context.Background(), io.Discard); err != nil {
		t.Fatal("completed embedded data must be adopted on retry:", err)
	}
	restoredCount := 0
	for _, call := range s.Calls() {
		if strings.Contains(call, "/containers/create") {
			restoredCount++
		}
	}
	if restoredCount != 1 {
		t.Fatal("resumed completed app data restore recreated or reset its volume")
	}
}

func TestRecoveryMachineCatalogArchivesImmutableLoadedImageIDs(t *testing.T) {
	r, _ := recoveryFixture(t)
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	original := state.Revision{App: m.App, ID: "source-original", Manifest: body, Images: map[string]string{"web": "source-registry:5000/image@digest"}, Containers: map[string]string{"web": "original-live-container"}, CreatedAt: time.Now()}
	if err := r.Store.SaveRevision(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Activate(context.Background(), m.App, original.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := r.Store.BackupTo(context.Background(), filepath.Join(stage, "state.db")); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	id := "sha256:" + strings.Repeat("b", 64)
	script := "#!/bin/sh\nif [ \"$1\" = image ] && [ \"$2\" = save ]; then printf archive > \"$4\"; else printf '%s\\n' '" + id + "'; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := Backup{Store: r.Store, Secrets: r.Secrets, StateDir: r.StateDir}
	if err := b.prepareMachineRecoveryCatalog(context.Background(), "old-host", stage, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(stage, machineCatalogFile))
	if err != nil {
		t.Fatal(err)
	}
	var c machineRecoveryCatalog
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Revisions) != 1 || c.Revisions[0].Images["web"] != id || !reflect.DeepEqual(c.Images, []string{id}) || !c.EmbeddedData {
		t.Fatal("new machine backup did not preserve archive-loadable immutable images and self-contained data format")
	}
	if _, err := os.Stat(filepath.Join(stage, machineImagesFile)); err != nil {
		t.Fatal("workload image archive missing")
	}
	history, err := r.Store.GetRevision(context.Background(), m.App, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if history.Images["web"] != original.Images["web"] {
		t.Fatal("machine catalog mutated the original source revision")
	}
}
