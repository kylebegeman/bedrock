// Package host inspects and prepares the machine quark runs on: facts about
// it, the doctor's verdicts, and the operations that set it up, maintain it
// and upgrade quark itself.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/version"
)

// Env is how the host package touches the machine, so tests can hand it a
// fake machine instead.
type Env struct {
	// Run executes a command and returns its combined output, trimmed.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Root is prefixed to every file path: "/" on a real machine.
	Root string
	// Privileged is whether this process is root.
	Privileged bool
	// Reboot restarts the machine and, on a real one, never returns: the
	// process dies with the reboot and the operation resumes afterwards.
	Reboot func(ctx context.Context) error
	// RunningVersion is the build this process is.
	RunningVersion func() version.Info
}

// RealEnv is the machine this process runs on.
func RealEnv() Env {
	return Env{Run: runCommand, Root: "/", Privileged: os.Geteuid() == 0, Reboot: reboot, RunningVersion: version.Current}
}

// reboot asks systemd to reboot and waits to be killed. If the machine is
// still up after ten minutes, the reboot didn't happen.
func reboot(ctx context.Context) error {
	if _, err := runCommand(ctx, "systemctl", "reboot"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Minute):
		return errors.New("the machine didn't reboot within ten minutes")
	}
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return text, fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), exit.ExitCode(), lastLine(text))
		}
		return text, fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Path maps a machine path onto the environment's root.
func (e Env) Path(p string) string { return filepath.Join(e.Root, p) }

// ReadFile reads a machine file.
func (e Env) ReadFile(p string) (string, error) {
	b, err := os.ReadFile(e.Path(p))
	return string(b), err
}

// Exists reports whether a machine path exists.
func (e Env) Exists(p string) bool {
	_, err := os.Stat(e.Path(p))
	return err == nil
}

// WriteFile writes a machine file, creating its directory, and reports
// whether the content changed.
func (e Env) WriteFile(p string, content string, mode os.FileMode) (bool, error) {
	full := e.Path(p)
	if current, err := os.ReadFile(full); err == nil && string(current) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	tmp := full + ".quark-tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}
