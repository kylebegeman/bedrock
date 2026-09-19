package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/api"
)

// UnitPath is where the daemon's systemd unit lives.
const UnitPath = "/etc/systemd/system/quark.service"

const unitTemplate = `[Unit]
Description=Quark host daemon
Documentation=https://github.com/kylebegeman/quark
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=%s daemon run
Restart=always
RestartSec=2
WatchdogSec=30
StateDirectory=quark
StateDirectoryMode=0700
RuntimeDirectory=quark
RuntimeDirectoryMode=0750
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
`

// Install writes the unit for this binary, enables it, starts it, and waits
// for the daemon to answer. It needs root and systemd.
func Install(ctx context.Context, out io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing the daemon needs root")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return fmt.Errorf("this machine doesn't run systemd")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	unit := fmt.Sprintf(unitTemplate, exe)
	current, _ := os.ReadFile(UnitPath)
	if string(current) != unit {
		if err := os.WriteFile(UnitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n", UnitPath)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "quark.service"}, {"restart", "quark.service"}} {
		if err := systemctl(ctx, args...); err != nil {
			return err
		}
	}
	client := api.Dial(DefaultSocket)
	defer client.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if info, err := client.Version(ctx); err == nil {
			fmt.Fprintf(out, "quark daemon %s is running\n", info.Version)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon didn't answer on %s within 20s; see journalctl -u quark", DefaultSocket)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Status reports the unit's state and whether the daemon answers.
func Status(ctx context.Context, socket string) (string, error) {
	var b strings.Builder
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		outp, _ := exec.CommandContext(ctx, "systemctl", "show", "quark.service", "--property=ActiveState,SubState,NRestarts,MainPID").Output()
		props := map[string]string{}
		for _, line := range strings.Split(string(outp), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if props["ActiveState"] != "" {
			fmt.Fprintf(&b, "unit %s (%s), pid %s, restarts %s\n", props["ActiveState"], props["SubState"], props["MainPID"], props["NRestarts"])
		}
	}
	client := api.Dial(socket)
	defer client.Close()
	info, err := client.Version(ctx)
	if err != nil {
		fmt.Fprintf(&b, "daemon not answering on %s: %v", socket, err)
		return b.String(), err
	}
	fmt.Fprintf(&b, "daemon %s answering on %s", info.Version, socket)
	return b.String(), nil
}

func systemctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	outp, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(outp)))
	}
	return nil
}
