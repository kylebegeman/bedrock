package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apps "github.com/kylebegeman/bedrock/internal/app"
)

func TestRehearsalReportShowsExactEvidenceAndRejectsPathTraversal(t *testing.T) {
	stateDir := t.TempDir()
	proof := apps.RehearsalProof{App: "site", Revision: "candidate-rev", ActiveRevision: "active-rev", Status: "passed", SourceSHA256: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64), Snapshot: strings.Repeat("c", 64), Images: map[string]string{"web": "image@sha256:fixed"}}
	path := apps.RehearsalProofPath(stateDir, proof.App, proof.Revision)
	os.MkdirAll(filepath.Dir(path), 0o700)
	data, _ := json.Marshal(proof)
	os.WriteFile(path, data, 0o600)
	out, errOut, code := run(t, stateDir, "rehearse", "report", proof.App, proof.Revision)
	if code != 0 || !strings.Contains(out, proof.SourceSHA256) || !strings.Contains(out, proof.Snapshot) || !strings.Contains(out, "web: image@sha256:fixed") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "--json", "rehearse", "report", proof.App, proof.Revision)
	var parsed apps.RehearsalProof
	if code != 0 || json.Unmarshal([]byte(out), &parsed) != nil || parsed.SourceSHA256 != proof.SourceSHA256 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	for _, app := range []string{"../site", "bedrock"} {
		if _, _, code := run(t, stateDir, "rehearse", "report", app, proof.Revision); code == 0 {
			t.Fatalf("invalid app admitted: %s", app)
		}
	}
}
