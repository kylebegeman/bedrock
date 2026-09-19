package gitdeploy

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/manifest"
)

// MaxSource bounds an uploaded or exported source tree.
const MaxSource int64 = 2 << 30

// NewBuildDir makes the directory one deploy's source is written to.
func NewBuildDir(root, app, label string) (string, error) {
	dir := filepath.Join(root, app, fmt.Sprintf("%s-%d", label, time.Now().UnixMilli()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// PruneBuilds keeps an app's newest few source trees: the images are in
// the registry, so older trees are only disk.
func PruneBuilds(root, app string, keep int) {
	entries, err := os.ReadDir(filepath.Join(root, app))
	if err != nil {
		return
	}
	type dir struct {
		name string
		at   time.Time
	}
	var dirs []dir
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() {
			dirs = append(dirs, dir{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].at.After(dirs[j].at) })
	for i := keep; i < len(dirs); i++ {
		_ = os.RemoveAll(filepath.Join(root, app, dirs[i].name))
	}
}

// ExportCommit writes a commit's tree into dest, as git archive gives it.
// In a pre-receive hook the pushed objects are still in quarantine, and
// git finds them through the hook's environment, which this inherits.
func ExportCommit(ctx context.Context, repoDir, commit, dest string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "archive", "--format=tar", commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := Extract(stdout, dest)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("git archive %s: %s", commit, strings.TrimSpace(stderr.String()))
	}
	return extractErr
}

// Extract unpacks a tar stream, gzipped or not, into dest. It writes
// regular files, directories and symlinks that stay inside dest, and
// nothing else.
func Extract(r io.Reader, dest string) error {
	br := bufio.NewReader(r)
	var in io.Reader = br
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer gz.Close()
		in = gz
	}
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	tr := tar.NewReader(in)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the source archive: %w", err)
		}
		name := filepath.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." || name == "pax_global_header" {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("the source archive has a path outside itself: %s", h.Name)
		}
		target := filepath.Join(root, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > MaxSource {
				return fmt.Errorf("the source is larger than %d GiB", MaxSource>>30)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, h.Size)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := h.Linkname
			resolved := link
			if !filepath.IsAbs(link) {
				resolved = filepath.Join(filepath.Dir(target), link)
			}
			if filepath.IsAbs(link) || !strings.HasPrefix(filepath.Clean(resolved)+string(filepath.Separator), root+string(filepath.Separator)) {
				// A link out of the tree would let a build read the
				// machine; it is left out.
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		}
	}
}

// CheckSource makes sure a tree is the app it is deployed as: a real
// quark.yaml at its root naming that app.
func CheckSource(dir, app string) (*manifest.Manifest, error) {
	info, err := os.Lstat(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return nil, fmt.Errorf("the source has no %s at its root", manifest.FileName)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a plain file", manifest.FileName)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	if m.App != app {
		return nil, fmt.Errorf("%s says app: %s, but this is %s's remote", manifest.FileName, m.App, app)
	}
	return m, nil
}
