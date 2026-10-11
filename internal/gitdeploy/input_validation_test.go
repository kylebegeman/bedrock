package gitdeploy

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRepositoryValidationRejectsMalformedAndCredentialURLs(t *testing.T) {
	for _, repo := range []string{"https://", "https:///repo.git", "https://user:private-password@host.invalid/repo.git", "ssh://git:private-password@host.invalid/repo.git", "ssh://host.invalid:65536/repo.git", "file:///", "git@host.invalid:", "git@host.invalid:repo.git\nsecond"} {
		err := ValidRepo(repo)
		if err == nil {
			t.Errorf("invalid repository accepted: %q", repo)
		} else if strings.Contains(err.Error(), "private-password") {
			t.Error("repository rejection disclosed a credential")
		}
	}
}

func TestBranchValidationMatchesGitRefRules(t *testing.T) {
	for _, branch := range []string{"HEAD", "a//b", "a/.hidden", "a.lock/b", "branch."} {
		if err := ValidBranch(branch); err == nil {
			t.Errorf("invalid git branch accepted: %q", branch)
		}
	}
	for _, branch := range []string{"main", "feature/new-ui", "release/v1.2", "topic_name"} {
		if err := ValidBranch(branch); err != nil {
			t.Errorf("valid branch rejected: %q %v", branch, err)
		}
	}
}

func TestMalformedHookInputFailsWithoutPanicOrDeploy(t *testing.T) {
	var out bytes.Buffer
	// A truncated object ID must be rejected before slicing a display hash.
	if code := Hook(context.Background(), "hello", strings.NewReader("old a refs/heads/main\n"), &out, nil); code != 1 {
		t.Fatalf("malformed hook update accepted: code %d %s", code, out.String())
	}
}
