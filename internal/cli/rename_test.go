package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeliosAndLegacyEnvironmentSelectTheSameInstallation(t *testing.T) {
	for _, prefix := range []string{"HELIOS_", "BEDROCK_"} {
		t.Run(prefix, func(t *testing.T) {
			for _, suffix := range []string{"STATE_DIR", "SOCKET", "TARGETS_FILE", "BOOTSTRAP_DIR", "SSH"} {
				t.Setenv("HELIOS_"+suffix, "")
				t.Setenv("BEDROCK_"+suffix, "")
			}
			state := filepath.Join(t.TempDir(), "state")
			targets := filepath.Join(t.TempDir(), "targets.json")
			t.Setenv(prefix+"STATE_DIR", state)
			t.Setenv(prefix+"SOCKET", filepath.Join(state, "no.sock"))
			t.Setenv(prefix+"TARGETS_FILE", targets)
			var out, stderr bytes.Buffer
			if code := Main([]string{"target", "set", "staging", "--ssh", "fixture-host"}, &out, &stderr); code != 0 {
				t.Fatalf("%d %s", code, stderr.String())
			}
			if _, err := os.Stat(targets); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			stderr.Reset()
			if code := Main([]string{"kernel", "exercise", "--dir", filepath.Join(t.TempDir(), "markers"), "--steps", "1", "--yes"}, &out, &stderr); code != 0 {
				t.Fatalf("%d %s", code, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(state, "state.db")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConflictingEnvironmentAliasesFailBeforeWriting(t *testing.T) {
	for _, suffix := range []string{"STATE_DIR", "SOCKET", "TARGETS_FILE", "BOOTSTRAP_DIR", "SSH"} {
		t.Run(suffix, func(t *testing.T) {
			a, b := filepath.Join(t.TempDir(), "new"), filepath.Join(t.TempDir(), "old")
			t.Setenv("HELIOS_"+suffix, a)
			t.Setenv("BEDROCK_"+suffix, b)
			var out, stderr bytes.Buffer
			code := Main([]string{"version"}, &out, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), "conflict") || strings.Contains(stderr.String(), a) || strings.Contains(stderr.String(), b) {
				t.Fatalf("conflict was not safely refused: %d %s", code, stderr.String())
			}
			if _, err := os.Stat(a); !os.IsNotExist(err) {
				t.Fatal("new path was written")
			}
			if _, err := os.Stat(b); !os.IsNotExist(err) {
				t.Fatal("old path was written")
			}
		})
	}
}

func TestInitPreservesALegacyManifestInsteadOfCreatingAnotherDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bedrock.yaml")
	original := []byte("app: existing\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, t.TempDir(), "init", "hello", "--kind", "worker", "--dir", dir)
	if code == 0 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("existing file not detected: %d %s", code, stderr)
	}
	if body, _ := os.ReadFile(path); !bytes.Equal(body, original) {
		t.Fatal("legacy manifest changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "helios.yaml")); !os.IsNotExist(err) {
		t.Fatal("a second default was created")
	}
	_, stderr, code = run(t, t.TempDir(), "init", "hello", "--kind", "worker", "--dir", dir, "--force")
	if code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "helios.yaml")); !os.IsNotExist(err) {
		t.Fatal("force created an ambiguous second default")
	}
}

func TestCheckReportsTheActualLegacyManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bedrock.yaml"), []byte("app: hello\nworkloads:\n  worker:\n    kind: worker\n    image: alpine:3.21\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := run(t, t.TempDir(), "check", dir, "--json")
	var report sourceCheckReport
	if code != 0 || json.Unmarshal([]byte(out), &report) != nil || report.Manifest != "bedrock.yaml" || !report.Valid {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}

func TestInitForceCannotFollowAnExistingManifestSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "keep.yaml")
	if err := os.WriteFile(outside, []byte("keep these bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "bedrock.yaml")); err != nil {
		t.Fatal(err)
	}
	_, _, code := run(t, t.TempDir(), "init", "hello", "--kind", "worker", "--dir", dir, "--force")
	if code == 0 {
		t.Fatal("--force accepted a symlink manifest")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep these bytes" {
		t.Fatal("init wrote outside its source")
	}
}

func TestNamedTargetReceivesTheSelectedDefaultManifestExplicitly(t *testing.T) {
	for _, name := range []string{"helios.yaml", "bedrock.yaml"} {
		t.Run(name, func(t *testing.T) {
			config, _, captured := targetArtifactFixture(t)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, name), []byte("app: hello\nworkloads:\n  worker:\n    kind: worker\n    image: alpine:3.21\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, stderr, code := targetRun(t, config, "deploy", source, "--plan", "--json")
			if code != 0 {
				t.Fatal(stderr)
			}
			argv, err := os.ReadFile(filepath.Join(captured, "plan-args"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(argv), "--manifest="+name+"\n") {
				t.Fatalf("target default can differ from the client; selected name was not forwarded: %s", argv)
			}
		})
	}
}
