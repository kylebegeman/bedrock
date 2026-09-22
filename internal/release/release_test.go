package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
