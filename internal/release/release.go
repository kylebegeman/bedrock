// Package release fetches a published bedrock build for this machine.
//
// A machine that can upgrade itself is a machine that downloads something
// and then executes it, so nothing here is trusted on the strength of where
// it came from. A downloaded binary is written to a private directory,
// checksummed, and only made executable once the checksum matches.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "https://github.com/kylebegeman/bedrock"

// repo is Repo, except in tests, which serve a release of their own.
var repo = Repo

// maxAsset bounds a download. The binary is about 15 MB; anything an order
// of magnitude past that is not the thing we asked for.
const maxAsset = 256 << 20

var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// CheckVersion refuses anything that isn't a published release number, so a
// caller can say no before it creates directories or opens the network.
func CheckVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("%q isn't a release version such as 0.7.4", version)
	}
	return nil
}

// AssetName is what a release calls the build for an operating system and
// architecture, as build-release.sh names it.
func AssetName(version, goos, goarch string) string {
	return fmt.Sprintf("bedrock_%s_%s_%s", version, goos, goarch)
}

// Fetch downloads the given version's build for this machine into dir and
// returns its path.
//
// sha256Hex pins the expected checksum. When it is empty the checksum comes
// from the SHA256SUMS published beside the binary, which proves the two
// agree and nothing more: anyone who can replace one can replace both. Pass
// a checksum from reviewed source when that distinction matters.
func Fetch(ctx context.Context, version, dir, sha256Hex string) (string, error) {
	if err := CheckVersion(version); err != nil {
		return "", err
	}
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/releases/download/v%s", repo, version)

	want := strings.ToLower(strings.TrimSpace(sha256Hex))
	if want == "" {
		sums, err := get(ctx, base+"/SHA256SUMS", 1<<20)
		if err != nil {
			return "", fmt.Errorf("SHA256SUMS for %s: %w", version, err)
		}
		if want, err = sumFor(string(sums), asset); err != nil {
			return "", err
		}
	}
	if len(want) != 64 {
		return "", fmt.Errorf("%q isn't a sha256 checksum", sha256Hex)
	}

	// Not executable yet: a file that has not been checked must not be
	// runnable, even for the moment between writing and verifying it.
	path := filepath.Join(dir, asset)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	body, err := open(ctx, base+"/"+asset)
	if err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("%s: %w", asset, err)
	}
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(body, maxAsset))
	body.Close()
	closeErr := f.Close()
	switch {
	case err != nil:
		os.Remove(path)
		return "", fmt.Errorf("%s: %w", asset, err)
	case closeErr != nil:
		os.Remove(path)
		return "", closeErr
	case n >= maxAsset:
		os.Remove(path)
		return "", fmt.Errorf("%s is larger than %d bytes", asset, int64(maxAsset))
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		os.Remove(path)
		return "", fmt.Errorf("%s does not match its checksum: got %s, want %s", asset, got, want)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// sumFor reads one asset's checksum out of a SHA256SUMS file.
func sumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no %s; this release may not build for %s/%s", asset, runtime.GOOS, runtime.GOARCH)
}

func open(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("not published at %s", url)
		}
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return resp.Body, nil
}

func get(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, limit))
}
