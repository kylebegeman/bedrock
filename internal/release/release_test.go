package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serve stands in for the release page: the asset, and the sums beside it.
func serve(t *testing.T, version string, body []byte, sums string) *httptest.Server {
	t.Helper()
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	prefix := "/releases/download/v" + version
	mux.HandleFunc(prefix+"/"+asset, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	mux.HandleFunc(prefix+"/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, sums) })
	srv := httptest.NewServer(mux)
	old := Repo
	repo = srv.URL
	t.Cleanup(func() { repo = old; srv.Close() })
	return srv
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestFetchVerifiesAndMakesRunnable(t *testing.T) {
	body := []byte("#!/bin/sh\necho bedrock\n")
	asset := AssetName("0.7.4", runtime.GOOS, runtime.GOARCH)
	serve(t, "0.7.4", body, sumOf(body)+"  "+asset+"\n")

	path, err := Fetch(context.Background(), "0.7.4", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(body) {
		t.Fatalf("%v %q", err, got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("a verified binary has to be runnable")
	}
}

// The whole point of the checksum is that a wrong one stops the upgrade,
// and that nothing runnable is left behind when it does.
func TestFetchRefusesAndLeavesNothingWhenTheChecksumIsWrong(t *testing.T) {
	body := []byte("not the binary you asked for")
	asset := AssetName("0.7.4", runtime.GOOS, runtime.GOARCH)
	serve(t, "0.7.4", body, strings.Repeat("a", 64)+"  "+asset+"\n")

	dir := t.TempDir()
	if _, err := Fetch(context.Background(), "0.7.4", dir, ""); err == nil {
		t.Fatal("a mismatched checksum must refuse")
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 0 {
		t.Fatalf("a refused download left %d file(s) behind", len(left))
	}
}

// A caller who pins a checksum from reviewed source must not have it
// silently replaced by whatever the release publishes.
func TestAPinnedChecksumIsTheOneThatCounts(t *testing.T) {
	body := []byte("the real binary")
	asset := AssetName("0.7.4", runtime.GOOS, runtime.GOARCH)
	// The published sums say something else entirely.
	serve(t, "0.7.4", body, strings.Repeat("b", 64)+"  "+asset+"\n")

	if _, err := Fetch(context.Background(), "0.7.4", t.TempDir(), sumOf(body)); err != nil {
		t.Fatalf("the pinned checksum matches the bytes: %v", err)
	}
	if _, err := Fetch(context.Background(), "0.7.4", t.TempDir(), strings.Repeat("c", 64)); err == nil {
		t.Fatal("a pinned checksum that does not match must refuse")
	}
}

func TestFetchRefusesWhatItCannotAskFor(t *testing.T) {
	body := []byte("x")
	serve(t, "0.7.4", body, sumOf(body)+"  "+AssetName("0.7.4", runtime.GOOS, runtime.GOARCH)+"\n")
	for _, version := range []string{"", "latest", "v0.7.4", "0.7.4-dev", "../../etc"} {
		if _, err := Fetch(context.Background(), version, t.TempDir(), ""); err == nil {
			t.Fatalf("%q must be refused", version)
		}
	}
	if _, err := Fetch(context.Background(), "0.7.4", t.TempDir(), "nonsense"); err == nil {
		t.Fatal("a checksum that isn't one must be refused")
	}
}

// A release that has no build for this machine should say so, rather than
// failing later on a 404 body written to disk.
func TestFetchSaysWhenTheReleaseHasNoBuildForThisMachine(t *testing.T) {
	serve(t, "0.7.4", []byte("x"), "deadbeef  bedrock_0.7.4_plan9_mips\n")
	_, err := Fetch(context.Background(), "0.7.4", t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "lists no") {
		t.Fatalf("want a clear missing-asset error, got %v", err)
	}
}

func TestFetchSaysWhenTheVersionIsNotPublished(t *testing.T) {
	serve(t, "0.7.4", []byte("x"), "")
	_, err := Fetch(context.Background(), "0.9.9", t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("want a not-published error, got %v", err)
	}
}

func TestFetchDoesNotFollowAnExistingAssetSymlink(t *testing.T) {
	body := []byte("new binary")
	serve(t, "0.7.4", body, "")
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(victim, []byte("keep these bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(dir, AssetName("0.7.4", runtime.GOOS, runtime.GOARCH))
	if err := os.Symlink(victim, asset); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), "0.7.4", dir, sumOf(body)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "keep these bytes" {
		t.Fatalf("download overwrote symlink target: %q %v", got, err)
	}
}
func TestFailedFetchPreservesPreviousVerifiedAsset(t *testing.T) {
	serve(t, "0.7.4", []byte("wrong bytes"), "")
	dir := t.TempDir()
	asset := filepath.Join(dir, AssetName("0.7.4", runtime.GOOS, runtime.GOARCH))
	if err := os.WriteFile(asset, []byte("previous verified binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), "0.7.4", dir, strings.Repeat("c", 64)); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	got, err := os.ReadFile(asset)
	if err != nil || string(got) != "previous verified binary" {
		t.Fatalf("failed download destroyed previous asset: %q %v", got, err)
	}
}

func TestHeliosCanFetchHistoricalBedrockAssetsWithPublishedOrPinnedChecksums(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			body := []byte("historical verified binary")
			asset := fmt.Sprintf("bedrock_%s_%s_%s", "0.7.10", runtime.GOOS, runtime.GOARCH)
			mux := http.NewServeMux()
			mux.HandleFunc("/releases/download/v0.7.10/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
			mux.HandleFunc("/releases/download/v0.7.10/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
				if pinned {
					t.Error("pinned downloads must not require the published checksum file")
				}
				fmt.Fprintln(w, sumOf(body)+"  "+asset)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			previous := repo
			repo = srv.URL
			defer func() { repo = previous }()
			pin := ""
			if pinned {
				pin = sumOf(body)
			}
			path, err := Fetch(context.Background(), "0.7.10", t.TempDir(), pin)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(path) != asset {
				t.Fatalf("unexpected asset: %s", path)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
				t.Fatalf("download changed: %q %v", got, err)
			}
		})
	}
}

func TestAHeliosChecksumFailureCannotFallBackToAnotherAsset(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			mux := http.NewServeMux()
			legacyRequested := false
			mux.HandleFunc("/releases/download/v0.7.11/"+AssetName("0.7.11", runtime.GOOS, runtime.GOARCH), func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, "wrong candidate") })
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				legacyRequested = true
				fmt.Fprint(w, "fallback must not run")
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			previous := repo
			repo = srv.URL
			defer func() { repo = previous }()
			dir := t.TempDir()
			if _, err := Fetch(context.Background(), "0.7.11", dir, sumOf([]byte("the approved binary"))); err == nil {
				t.Fatal("failed candidate accepted")
			}
			if legacyRequested {
				t.Fatal("failure was bypassed through a different asset")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("failed download left files: %v %v", files, err)
			}
		})
	}
}
