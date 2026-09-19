package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// HelperImage runs small jobs against volumes: listing, restoring, chowning.
const HelperImage = "alpine:3.21"

// VolumeName names an app's volume.
func VolumeName(app, volume string) string { return "quark-" + app + "-" + volume }

// EnsureVolume creates a named volume if it doesn't exist.
func (e *Engine) EnsureVolume(ctx context.Context, name string) error {
	_, err := e.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: map[string]string{LabelOwner: OwnerValue}})
	if err != nil {
		return fmt.Errorf("volume %s: %w", name, err)
	}
	return nil
}

// ensureHelper pulls the helper image once.
func (e *Engine) ensureHelper(ctx context.Context) error {
	if e.HasImage(ctx, HelperImage) {
		return nil
	}
	return e.Pull(ctx, HelperImage, io.Discard)
}

// VolumeEmpty reports whether a volume holds nothing.
func (e *Engine) VolumeEmpty(ctx context.Context, name string) (bool, error) {
	if err := e.ensureHelper(ctx); err != nil {
		return false, err
	}
	out, err := cliOutput(ctx, nil, "docker", "run", "--rm", "-v", name+":/v:ro", HelperImage, "sh", "-c", "ls -A /v | head -1")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// RestoreVolume unpacks a .tar.gz into a volume.
func (e *Engine) RestoreVolume(ctx context.Context, name, tarball string) error {
	if err := e.ensureHelper(ctx); err != nil {
		return err
	}
	abs, err := filepath.Abs(tarball)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	args := []string{"run", "--rm", "-v", name + ":/v", "-v", filepath.Dir(abs) + ":/backup:ro", HelperImage,
		"tar", "xzf", "/backup/" + filepath.Base(abs), "-C", "/v", "--no-same-owner"}
	// An archive of a Docker volume's directory itself (tar -C volumes/x
	// _data) wraps everything in _data/; unwrap it.
	if wrappedInData(ctx, abs) {
		args = append(args, "--strip-components=1")
	}
	if _, err := cliOutput(ctx, nil, "docker", args...); err != nil {
		return fmt.Errorf("restore %s: %w", name, err)
	}
	return nil
}

// wrappedInData reports whether every entry of a tarball sits under _data/.
func wrappedInData(ctx context.Context, tarball string) bool {
	out, err := cliOutput(ctx, nil, "tar", "tzf", tarball)
	if err != nil || out == "" {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if line != "_data/" && !strings.HasPrefix(line, "_data/") {
			return false
		}
	}
	return true
}

// ChownVolume makes a volume's files belong to the user an image runs as,
// using that image so the user's name resolves. A numeric or empty user
// is handled too.
func (e *Engine) ChownVolume(ctx context.Context, name, image, user, path string) error {
	if user == "" {
		return nil
	}
	_, err := cliOutput(ctx, nil, "docker", "run", "--rm", "--user", "0", "--entrypoint", "chown", "-v", name+":"+path, image, "-R", user, path)
	if err != nil {
		return fmt.Errorf("chown %s: %w", name, err)
	}
	return nil
}

// ImageUser returns the user an image runs as, as its config says.
func (e *Engine) ImageUser(ctx context.Context, ref string) (string, error) {
	res, err := e.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	if res.Config == nil {
		return "", nil
	}
	return res.Config.User, nil
}

// ExecTo runs a command in a running container, streaming its stdout to
// w. Its stderr comes back as text, for errors.
func (e *Engine) ExecTo(ctx context.Context, container string, w io.Writer, cmd ...string) (string, error) {
	args := append([]string{"exec", container}, cmd...)
	c := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	c.Stdout, c.Stderr = w, &stderr
	err := c.Run()
	text := strings.TrimSpace(stderr.String())
	if err != nil {
		lines := strings.Split(text, "\n")
		return text, fmt.Errorf("%s: %s", err, lines[len(lines)-1])
	}
	return text, nil
}

// VolumeExists reports whether a volume is there.
func (e *Engine) VolumeExists(ctx context.Context, name string) bool {
	_, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	return err == nil
}

// VolumeMountpoint is where a volume's files live on the machine.
func (e *Engine) VolumeMountpoint(ctx context.Context, name string) (string, error) {
	res, err := e.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return "", err
	}
	return res.Volume.Mountpoint, nil
}

// Exec runs a command in a running container and returns its output.
func (e *Engine) Exec(ctx context.Context, container string, stdin io.Reader, cmd ...string) (string, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(append(args, container), cmd...)
	return cliOutput(ctx, stdin, "docker", args...)
}

// ExitCode returns a stopped container's exit code, or -1 while it runs.
func (e *Engine) ExitCode(ctx context.Context, name string) (int, bool, error) {
	res, err := e.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return 0, false, err
	}
	if res.Container.State == nil || res.Container.State.Running {
		return -1, false, nil
	}
	return res.Container.State.ExitCode, true, nil
}

// WaitExit polls until a container stops or the context ends.
func (e *Engine) WaitExit(ctx context.Context, name string) (int, error) {
	for {
		code, done, err := e.ExitCode(ctx, name)
		if err != nil {
			return -1, err
		}
		if done {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// LogTail returns the last lines of a container's output.
func (e *Engine) LogTail(ctx context.Context, name string, lines int) string {
	var buf bytes.Buffer
	_ = e.Logs(ctx, name, false, fmt.Sprint(lines), &buf)
	return buf.String()
}

func cliOutput(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		lines := strings.Split(text, "\n")
		return text, fmt.Errorf("%s: %s", err, lines[len(lines)-1])
	}
	return text, nil
}

// RemoveVolume deletes a volume and reports whether there was one.
func (e *Engine) RemoveVolume(ctx context.Context, name string) (bool, error) {
	_, err := e.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	if IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove volume %s: %w", name, err)
	}
	return true, nil
}

// RemoveNetwork deletes a network. Missing is fine.
func (e *Engine) RemoveNetwork(ctx context.Context, name string) error {
	_, err := e.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	return nil
}
