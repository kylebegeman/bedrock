package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/remote"
)

func snapshotSource(ctx context.Context, source string) (*os.File, string, error) {
	dir, err := filepath.Abs(source)
	if err != nil {
		return nil, "", err
	}
	files, err := sourceFiles(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	// Environment files belong in the secret store, even in a non-Git tree.
	selected := files[:0]
	for _, file := range files {
		name := filepath.Base(file)
		if name == ".env" || strings.HasPrefix(name, ".env.") {
			continue
		}
		selected = append(selected, file)
	}
	files = selected
	f, err := os.CreateTemp("", "bedrock-source-*.tar.gz")
	if err != nil {
		return nil, "", err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = writeTree(&artifactLimitWriter{Writer: f, Remaining: gitdeploy.MaxSource}, dir, files); err != nil {
		return nil, "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	sum := sha256.New()
	if _, err = io.Copy(sum, io.LimitReader(f, gitdeploy.MaxSource+1)); err != nil {
		return nil, "", err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, "", err
	}
	if size > gitdeploy.MaxSource {
		return nil, "", errors.New("source archive exceeds 2 GiB")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	ok = true
	return f, hex.EncodeToString(sum.Sum(nil)), nil
}
func snapshotFile(path string) (*os.File, string, error) { return snapshotBoundedFile(path, 512<<20) }

func snapshotBoundedFile(path string, max int64) (*os.File, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, max+1))
	if err != nil || n > max {
		f.Close()
		if err == nil {
			err = errors.New("artifact exceeds size limit")
		}
		return nil, "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", err
	}
	return f, hex.EncodeToString(sum.Sum(nil)), nil
}
func (a *app) targetStateArgs() []string {
	if a.remoteStateFlags != nil {
		return append([]string{}, a.remoteStateFlags...)
	}
	var args []string
	if a.stateDir != defaultStateDir() {
		args = append(args, "--state-dir="+a.stateDir)
	}
	if a.socket != "" && a.socket != "/run/bedrock/bedrock.sock" {
		args = append(args, "--socket="+a.socket)
	}
	return args
}
func (a *app) uploadArtifact(ctx context.Context, t remote.Target, f *os.File, hash, kind string) (string, error) {
	args := append(a.targetStateArgs(), "_remote-artifact", "--sha256="+hash, "--kind="+kind)
	var out strings.Builder
	if err := targetTransport().Run(ctx, t, args, f, &out, a.stderr); err != nil {
		return "", fmt.Errorf("upload to target: %w", err)
	}
	path := strings.TrimSpace(out.String())
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\n\r\x00") {
		return "", errors.New("target returned invalid artifact path")
	}
	return path, nil
}

var artifactHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func newRemoteArtifact(a *app) *cobra.Command {
	var hash, kind string
	cmd := &cobra.Command{Use: "_remote-artifact", Hidden: true, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		if !artifactHash.MatchString(hash) {
			return errors.New("invalid artifact SHA-256")
		}
		if kind != "source" && kind != "binary" && kind != "data" {
			return errors.New("artifact kind must be source, binary or data")
		}
		root := filepath.Join(a.stateDir, "remote-artifacts")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		incoming, err := os.CreateTemp(root, ".upload-*")
		if err != nil {
			return err
		}
		defer os.Remove(incoming.Name())
		defer incoming.Close()
		max := gitdeploy.MaxSource
		if kind == "binary" {
			max = 512 << 20
		}
		sum := sha256.New()
		n, err := io.Copy(io.MultiWriter(incoming, sum), io.LimitReader(os.Stdin, max+1))
		if err != nil {
			return err
		}
		if n > max {
			return errors.New("artifact exceeds size limit")
		}
		if hex.EncodeToString(sum.Sum(nil)) != hash {
			return errors.New("artifact checksum mismatch")
		}
		dest := filepath.Join(root, kind+"-"+hash)
		result := dest
		if kind == "source" {
			result = filepath.Join(dest, "source")
		}
		if info, err := os.Lstat(dest); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("artifact destination is a symlink")
			}
			fmt.Fprintln(a.stdout, result)
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if kind != "source" {
			mode := os.FileMode(0o600)
			if kind == "binary" {
				mode = 0o755
			}
			if err := incoming.Chmod(mode); err != nil {
				return err
			}
			if err := incoming.Sync(); err != nil {
				return err
			}
			if err := incoming.Close(); err != nil {
				return err
			}
			if err := os.Rename(incoming.Name(), dest); err != nil {
				return err
			}
		} else {
			temp, err := os.MkdirTemp(root, ".source-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(temp)
			unpack := filepath.Join(temp, "source")
			if err := os.MkdirAll(unpack, 0o755); err != nil {
				return err
			}
			if _, err := incoming.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := gitdeploy.Extract(incoming, unpack); err != nil {
				return err
			}
			if err := os.Rename(temp, dest); err != nil {
				if _, exists := os.Stat(result); exists != nil {
					return err
				}
			}
		}
		fmt.Fprintln(a.stdout, result)
		return nil
	}}
	cmd.Flags().StringVar(&hash, "sha256", "", "expected artifact hash")
	cmd.Flags().StringVar(&kind, "kind", "", "source, binary or data")
	return cmd
}

type artifactLimitWriter struct {
	io.Writer
	Remaining int64
}

func (w *artifactLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.Remaining {
		return 0, errors.New("source archive exceeds size limit")
	}
	n, err := w.Writer.Write(p)
	w.Remaining -= int64(n)
	return n, err
}

func openRecoveryIdentityFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("recovery identity must be a plain file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("recovery identity file must be readable only by its owner; chmod 600 %s", shellArgument(path))
	}
	return os.Open(path)
}
