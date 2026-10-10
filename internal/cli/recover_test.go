package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/secrets"
)

func TestRecoveryIdentityFileMustBePrivateAndCannotBeASymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.key")
	if err := os.WriteFile(path, []byte("private identity input"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryIdentityFile(path); err == nil {
		t.Fatal("group/world readable identity files must refuse")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openRecoveryIdentityFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryIdentityFile(link); err == nil {
		t.Fatal("identity file symlinks must refuse")
	}
}

func TestRecoveryPlanDoesNotReadIdentityOrCreateAProtectedArtifact(t *testing.T) {
	dir := t.TempDir()
	sec := secrets.DefaultStore(dir)
	if _, _, _, err := sec.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := integration.Set(sec, integration.StorageName, map[string]string{"kind": "s3", "endpoint": "https://storage.example.test", "key_id": "key-id", "key": "storage-value", "bucket_prefix": "target", "password": "backup-password"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{stateDir: dir, socket: filepath.Join(dir, "absent.sock"), stdout: &out, stderr: &out}
	cmd := newRecover(a)
	cmd.SetArgs([]string{"old-host", "--snapshot", strings.Repeat("a", 64), "--identity-file", filepath.Join(dir, "missing-source.key"), "--plan"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "machine-recovery")); !os.IsNotExist(err) {
		t.Fatal("--plan created recovery workspace or sealed identity")
	}
	if !strings.Contains(out.String(), "old-host") || !strings.Contains(out.String(), "snapshot") {
		t.Fatal("plan omitted exact recovery selection")
	}
}

func TestRecoveryStdinCannotBeConsumedByTheApprovalPrompt(t *testing.T) {
	var out bytes.Buffer
	a := &app{stateDir: t.TempDir(), stdout: &out, stderr: &out}
	cmd := newRecover(a)
	cmd.SetIn(strings.NewReader("private source identity input"))
	cmd.SetArgs([]string{"old-host", "--snapshot", strings.Repeat("a", 64), "--identity-stdin"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stdin is reserved") {
		t.Fatal("identity stdin must not fall through to a terminal approval prompt")
	}
	if strings.Contains(out.String(), "private source identity input") {
		t.Fatal("stdin key appeared in output")
	}
}
