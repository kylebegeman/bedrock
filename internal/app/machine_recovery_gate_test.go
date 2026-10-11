package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/restic"
	"github.com/kylebegeman/bedrock/internal/state"
)

func resticSnapshot(id string) restic.Snapshot { return restic.Snapshot{ID: id} }

func TestRecoveryReservationAllowsOnlyItsBoundRetryAndCutover(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	rec := &machineRecoveryRecord{Input: in, Snapshot: resticSnapshot(in.Snapshot)}
	if err := x.reserve(rec); err != nil {
		t.Fatal(err)
	}
	if err := x.reserve(rec); err != nil {
		t.Fatal("reservation retry must be idempotent:", err)
	}
	raw, _ := json.Marshal(in)
	if err := CheckMachineRecoveryAccess(r.Store.Path(), RecoverMachineKind, raw); err != nil {
		t.Fatal(err)
	}
	cutover, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	for _, kind := range []string{RecoverCutoverKind, RecoverAbandonKind} {
		if err := CheckMachineRecoveryAccess(r.Store.Path(), kind, cutover); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{DeployKind, "host.setup", BackupKind, "app.run"} {
		if err := CheckMachineRecoveryAccess(r.Store.Path(), kind, nil); err == nil {
			t.Fatalf("reserved recovery must exclude %s", kind)
		}
	}
	if CheckMachineRecoveryWriteAccess(r.Store.Path()) == nil {
		t.Fatal("operator data writes must respect the durable recovery reservation")
	}
	other := in
	other.ID = "recover-another-id"
	otherRaw, _ := json.Marshal(other)
	if CheckMachineRecoveryAccess(r.Store.Path(), RecoverMachineKind, otherRaw) == nil {
		t.Fatal("a second recovery cannot adopt the reservation")
	}
	other = in
	other.Rehearsal = true
	otherRaw, _ = json.Marshal(other)
	if CheckMachineRecoveryAccess(r.Store.Path(), RecoverMachineKind, otherRaw) == nil {
		t.Fatal("rehearsal cannot bypass a real recovery reservation")
	}
	if err := x.releaseReservation(); err != nil {
		t.Fatal(err)
	}
	if CheckMachineRecoveryWriteAccess(r.Store.Path()) != nil {
		t.Fatal("completed cutover did not release machine writes")
	}
}

func TestRecoveryRehearsalNeverReservesTheHost(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(true)
	x := &machineRecovering{r: r, in: in}
	if err := x.reserve(&machineRecoveryRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(machineRecoveryGatePath(r.Store.Path())); !os.IsNotExist(err) {
		t.Fatal("disposable rehearsal reserved the host")
	}
}

func TestRecoveryAbandonPreservesDataAndUnblocksTheMachine(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, ResourcesClaimed: true, Snapshot: resticSnapshot(in.Snapshot), Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body}}}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	if err := x.reserve(rec); err != nil {
		t.Fatal(err)
	}
	s := dockertest.New(t)
	n := x.names(&m)
	s.Add(dockertest.Container{Name: n.containers["web"], Running: true, Labels: map[string]string{"bedrock.recovery": in.ID, docker.LabelOwner: docker.OwnerValue, docker.LabelApp: m.App, docker.LabelWorkload: "web", docker.LabelRevision: recoveryRevisionID(in.ID)}})
	def := RecoverAbandon{Recovery: r}
	raw, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	plan, err := def.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if err := step.Apply(context.Background(), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Container(n.containers["web"]); ok {
		t.Fatal("abandoned private container is still running")
	}
	for _, call := range s.Calls() {
		if strings.Contains(call, "DELETE /volumes/") {
			t.Fatal("default abandonment deleted restored data")
		}
	}
	if CheckMachineRecoveryWriteAccess(r.Store.Path()) != nil {
		t.Fatal("abandon left an orphaned machine reservation")
	}
	report, err := ReadMachineRecoveryReport(r.StateDir, in.ID)
	if err != nil || report.Status != "abandoned" {
		t.Fatal("abandonment is not visible in the recovery report")
	}
}

func TestRecoveryAbandonRefusesChangedOwnershipAndBegunCutover(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, ResourcesClaimed: true, Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body}}}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	s := dockertest.New(t)
	name := x.names(&m).containers["web"]
	s.Add(dockertest.Container{Name: name, Running: true, Labels: map[string]string{"bedrock.recovery": "someone-else"}})
	a := RecoverAbandon{Recovery: r}
	raw, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	plan, err := a.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Steps[0].Apply(context.Background(), io.Discard); err == nil {
		t.Fatal("changed resource ownership must refuse cleanup")
	}
	if _, ok := s.Container(name); !ok {
		t.Fatal("abandon deleted someone else's container")
	}
	rec.CutoverReady = map[string]bool{m.App: true}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Plan(context.Background(), raw); err == nil {
		t.Fatal("begun cutover must be resumed, never abandoned as private recovery")
	}
}

func TestRecoveryInstallStatePreservesSourceHistoryAndTargetJournal(t *testing.T) {
	r, sourceSecrets := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	source, err := state.Open(sourceSecrets.KeyPath + ".db")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	original := state.Revision{App: m.App, ID: "source-original", Status: state.RevisionActive, Manifest: body, Images: map[string]string{"web": "source-registry:5000/image@digest"}, Containers: map[string]string{"web": "source-private-name"}, CreatedAt: time.Now().UTC()}
	if err := source.SaveRevision(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if err := source.Activate(context.Background(), m.App, original.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(x.stage(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := source.BackupTo(context.Background(), x.stage()+"/state.db"); err != nil {
		t.Fatal(err)
	}
	catalogRev := original
	catalogRev.Images = map[string]string{"web": "sha256:" + strings.Repeat("b", 64)}
	rec := &machineRecoveryRecord{Input: in, StartedAt: time.Now().UTC(), Catalog: machineRecoveryCatalog{Revisions: []state.Revision{catalogRev}}, Ready: map[string]bool{m.App: true}, SecretVersions: map[string]map[int]int{}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	if err := x.installState(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	recovered, err := r.Store.GetRevision(context.Background(), m.App, recoveryRevisionID(in.ID))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status == state.RevisionActive || recovered.Images["web"] != catalogRev.Images["web"] || recovered.Containers["web"] == original.Containers["web"] {
		t.Fatal("recovery activated source containers or lost archived immutable images")
	}
	history, err := r.Store.GetRevision(context.Background(), m.App, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if history.Images["web"] != original.Images["web"] || len(history.Containers) != 0 {
		t.Fatal("source history was overwritten with new recovery images or remote container names")
	}
	// Model a crash after the state transaction but before saving its marker.
	rec.StateInstalled = false
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	if err := x.installState(context.Background(), io.Discard); err != nil {
		t.Fatal("post-import interrupted retry must adopt the exact content:", err)
	}
	imported, err := r.Store.MachineRecoveryImported(context.Background(), in.ID)
	if err != nil || !imported {
		t.Fatal("target lost recovery import marker")
	}
}

func TestRecoveryCutoverMarksItsStartBeforeRuntimeMutation(t *testing.T) {
	r, _ := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	rec := &machineRecoveryRecord{Input: in, FinishedAt: time.Now(), StateInstalled: true,
		Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body, Images: map[string]string{"web": "sha256:" + strings.Repeat("b", 64)}}}}, Ready: map[string]bool{m.App: true}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	plan, err := (RecoverCutover{Recovery: r}).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Name != "begin" {
		t.Fatal("cutover must durably mark its start before any Docker or DNS mutation")
	}
	if err := plan.Steps[0].Apply(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := (RecoverAbandon{Recovery: r}).Plan(context.Background(), raw); err == nil {
		t.Fatal("an interrupted begun cutover must refuse abandonment before the first app is ready")
	}
	report, err := ReadMachineRecoveryReport(r.StateDir, in.ID)
	if err != nil || report.Status != "cutover-incomplete" {
		t.Fatal("a begun interrupted cutover must be visible in the recovery report")
	}
}

func TestCompletedRecoveryCutoverDoesNotReplayLiveChanges(t *testing.T) {
	ctx := context.Background()
	r, _ := recoveryFixture(t)
	in := recoveryInput(false)
	x := &machineRecovering{r: r, in: in, dir: RecoveryDirectory(r.StateDir, in.ID)}
	m := recoveryManifest()
	body, _ := json.Marshal(m)
	recovered := state.Revision{App: m.App, ID: recoveryRevisionID(in.ID), Manifest: body, Status: state.RevisionFailed, CreatedAt: time.Now()}
	if err := r.Store.SaveRevision(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	newer := recovered
	newer.ID, newer.Status = "newer-live", state.RevisionActive
	if err := r.Store.SaveRevision(ctx, newer); err != nil {
		t.Fatal(err)
	}
	rec := &machineRecoveryRecord{Input: in, Snapshot: resticSnapshot(in.Snapshot), FinishedAt: time.Now(), StateInstalled: true, CutoverStarted: true, Cutover: true,
		Catalog: machineRecoveryCatalog{Revisions: []state.Revision{{App: m.App, Manifest: body, Images: map[string]string{"web": "sha256:" + strings.Repeat("b", 64)}}}}, Ready: map[string]bool{m.App: true}, CutoverReady: map[string]bool{m.App: true}}
	if err := x.save(rec); err != nil {
		t.Fatal(err)
	}
	// A crash after recording completion can leave the reservation behind.
	if err := x.reserve(rec); err != nil {
		t.Fatal(err)
	}
	fake := dockertest.New(t)
	raw, _ := json.Marshal(RecoverCutoverInput{ID: in.ID})
	plan, err := (RecoverCutover{Recovery: r}).Plan(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range plan.Steps {
		if err := step.Apply(ctx, io.Discard); err != nil {
			t.Fatalf("completed cutover replay %s: %v", step.Name, err)
		}
	}
	active, err := r.Store.RevisionWithStatus(ctx, m.App, state.RevisionActive)
	if err != nil || active == nil || active.ID != newer.ID {
		t.Fatalf("completed recovery reactivated stale code: %+v, %v", active, err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("completed recovery changed Docker: %v", fake.Calls())
	}
	if err := CheckMachineRecoveryWriteAccess(r.Store.Path()); err != nil {
		t.Fatalf("completed recovery did not release its orphaned reservation: %v", err)
	}
}
