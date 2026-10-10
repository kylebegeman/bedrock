package cli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/remote"
)

func targetArtifactFixture(t *testing.T) (string, string, string) {
	t.Helper()
	config, record := targetFixture(t)
	c, err := remote.Load(config)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("BEDROCK_TEST_ARTIFACT_DIR", dir)
	script := `#!/bin/sh
case "$*" in
 *capabilities*) printf '%s\n' '{"api_version":1,"manifest_version":1,"state_version":1,"machine_id":"fixture-machine","capabilities":["remote.source.v1","kernel.plan.v1","kernel.apply.v1","kernel.expect.v1"]}'; exit 0 ;;
 *_remote-artifact*)
  for arg do
   case "$arg" in --sha256=*) hash=${arg#*=} ;; --kind=*) kind=${arg#*=} ;; esac
  done
  cat > "$BEDROCK_TEST_ARTIFACT_DIR/$kind-$hash"
  suffix=""; test "$kind" != source || suffix=/source
  printf '/srv/artifacts/%s-%s%s\n' "$kind" "$hash" "$suffix"
  exit 0 ;;
 *--plan*) printf '%s\n' "$@" > "$BEDROCK_TEST_ARTIFACT_DIR/plan-args"
  printf '%s\n' '{"kind":"app.deploy","target":"fixture","recovery":"resume","steps":[{"index":0,"name":"one","change":"fixture change"}],"digest":"remote-plan"}'
  exit 0 ;;
 *check*) printf '%s\n' '{"schema_version":"1","source":"/srv/artifacts/source","manifest":"alternate.yaml","app":"fixture","valid":true,"diagnostics":[],"host_preflight":{"valid":true,"plan":{"kind":"app.deploy","target":"fixture","recovery":"resume","steps":[{"index":0,"name":"one","change":"fixture"}],"digest":"remote-plan"},"not_checked":[]}}'; exit 0 ;;
esac
printf '%s\n' "$@" > "$BEDROCK_TEST_REMOTE_ARGS"
cat > "$BEDROCK_TEST_ARTIFACT_DIR/applied-stdin"
printf 'remote applied\n'
`
	if err := os.WriteFile(c.Targets["one"].Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return config, record, dir
}

func targetSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"alternate.yaml": "app: fixture\n", "Dockerfile": "FROM scratch\n", ".env": "fixture private material"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRemoteDeployBindsSourceAndRestoreFiles(t *testing.T) {
	config, record, captured := targetArtifactFixture(t)
	source := targetSource(t)
	backup := filepath.Join(t.TempDir(), "data archive.tar.gz")
	if err := os.WriteFile(backup, []byte("fixture archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"deploy", source, "--manifest", "alternate.yaml", "--restore-volume", "data=" + backup}
	out, stderr, code := targetRun(t, config, append(args, "--plan", "--json")...)
	var plan kernel.PlanView
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	data, _ := os.ReadFile(filepath.Join(captured, "plan-args"))
	if !strings.Contains(string(data), "--manifest=alternate.yaml\n") || !strings.Contains(string(data), "--restore-volume=data=/srv/artifacts/data-") || strings.Contains(string(data), source) || strings.Contains(string(data), backup) {
		t.Fatalf("local paths or flags were not translated: %s", data)
	}
	files, err := filepath.Glob(filepath.Join(captured, "source-*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("source uploads %v %v", files, err)
	}
	f, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found[h.Name] = true
	}
	if !found["alternate.yaml"] || found[".env"] {
		t.Fatalf("source archive files %v", found)
	}
	_, stderr, code = targetRun(t, config, append(args, "--digest", plan.Digest)...)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	os.Remove(record)
	if err := os.WriteFile(backup, []byte("changed archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = targetRun(t, config, append(args, "--digest", plan.Digest)...)
	if code == 0 || !strings.Contains(stderr, "source snapshot") {
		t.Fatalf("stale archive approval accepted: %d %s", code, stderr)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("stale approval applied")
	}
}

func TestRemoteHostCheckScopesItsEmbeddedPlan(t *testing.T) {
	config, _, _ := targetArtifactFixture(t)
	source := targetSource(t)
	out, stderr, code := targetRun(t, config, "check", source, "--host", "--manifest", "alternate.yaml", "--json")
	var report sourceCheckReport
	if code != 0 || json.Unmarshal([]byte(out), &report) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	if report.Source != source || report.HostPreflight == nil || report.HostPreflight.Plan == nil || report.HostPreflight.Plan.Digest == "remote-plan" || !strings.Contains(report.HostPreflight.Plan.Digest, ".") {
		t.Fatalf("unscoped or confusing remote report %s", out)
	}
}
