package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/manifest"
)

func TestLegacyManifestIsCapturedAndRecoversAfterItsSourceDisappears(t *testing.T) {
	d, in := deploySourceFixture(t)
	if err := os.Rename(filepath.Join(in.Source, manifest.FileName), filepath.Join(in.Source, manifest.LegacyFileName)); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := d.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := plan.Steps[0].Apply(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(in.Source); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Plan(ctx, raw); err != nil {
		t.Fatalf("legacy captured source could not recover: %v", err)
	}
}

func TestRenamingAManifestChangesItsApproval(t *testing.T) {
	d, in := deploySourceFixture(t)
	registry := kernel.Registry{}
	registry.Add(d)
	engine := kernel.New(d.Store, registry, "test")
	raw, _ := json.Marshal(in)
	before, err := engine.PlanOnly(context.Background(), DeployKind, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(in.Source, manifest.FileName), filepath.Join(in.Source, manifest.LegacyFileName)); err != nil {
		t.Fatal(err)
	}
	after, err := engine.PlanOnly(context.Background(), DeployKind, raw)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest {
		t.Fatal("manifest rename retained a stale deployment approval")
	}
}

func TestWorkloadsKeepLegacyMetadataAndReceiveHeliosMetadata(t *testing.T) {
	m, err := manifest.Parse([]byte("app: hello\nworkloads:\n  worker:\n    kind: worker\n    image: alpine:3.21\n"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := containerSpec(m, "worker", m.Workloads["worker"], "candidate", "alpine:3.21", nil)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, entry := range spec.Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	for suffix, want := range map[string]string{"APP": "hello", "WORKLOAD": "worker", "REVISION": "candidate"} {
		if env["HELIOS_"+suffix] != want || env["BEDROCK_"+suffix] != want {
			t.Fatalf("metadata changed: %v", env)
		}
	}
	if spec.Name != "bedrock-hello-worker-candidate" {
		t.Fatalf("rename silently changed the runtime identity: %s", spec.Name)
	}
}
