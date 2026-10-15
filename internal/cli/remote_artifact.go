package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/helios/internal/gitdeploy"
	"github.com/kylebegeman/helios/internal/remote"
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
	var out bytes.Buffer
	if err := targetTransport().Run(ctx, t, args, f, &boundedBuffer{Buffer: &out, Max: 1 << 20}, a.stderr); err != nil {
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
		exists := false
		if info, err := os.Lstat(dest); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("artifact destination is a symlink")
			}
			exists = true
			if kind != "source" {
				if !info.Mode().IsRegular() {
					return errors.New("cached artifact is not a regular file")
				}
				cached, actual, err := snapshotBoundedFile(dest, max)
				if err != nil {
					return err
				}
				cached.Close()
				if actual != hash {
					return errors.New("cached artifact checksum changed; repair the cached artifact before retrying")
				}
				fmt.Fprintln(a.stdout, result)
				return nil
			}
			if !info.IsDir() {
				return errors.New("cached source artifact is not a directory")
			}
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
			if exists {
				expected, err := artifactTreeDigest(unpack)
				if err != nil {
					return err
				}
				actual, err := artifactTreeDigest(result)
				if err != nil {
					return err
				}
				if expected != actual {
					return errors.New("cached source changed; repair the cached artifact before retrying")
				}
			} else if err := os.Rename(temp, dest); err != nil {
				return err
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

// Extraction normalizes timestamps and non-executable permissions. Compare
// paths, types, executable modes, links and bytes when adopting cached source.
func artifactTreeDigest(dir string) (string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("cached source root must be a plain directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	sum := sha256.New()
	enc := json.NewEncoder(sum)
	var total int64
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := struct {
			Name       string
			Kind       string
			Executable bool
			Content    string
		}{Name: filepath.ToSlash(name), Executable: info.Mode().Perm()&0o111 != 0}
		switch {
		case info.IsDir():
			record.Kind = "directory"
			record.Executable = false
		case info.Mode()&os.ModeSymlink != 0:
			record.Kind = "symlink"
			record.Content, err = os.Readlink(path)
			if err != nil {
				return err
			}
		case info.Mode().IsRegular():
			record.Kind = "file"
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(hash, io.LimitReader(f, gitdeploy.MaxSource-total+1))
			f.Close()
			if copyErr != nil {
				return copyErr
			}
			total += n
			if total > gitdeploy.MaxSource {
				return errors.New("cached source exceeds size limit")
			}
			record.Content = hex.EncodeToString(hash.Sum(nil))
		default:
			return errors.New("cached source contains an unsupported file type")
		}
		return enc.Encode(record)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
