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

	"github.com/kylebegeman/bedrock/internal/api"
	"github.com/kylebegeman/bedrock/internal/host"
)

// UnitPath is where the daemon's systemd unit lives.
const UnitPath = "/etc/systemd/system/bedrock.service"

const unitTemplate = `[Unit]
Description=Bedrock host daemon
Documentation=https://github.com/kylebegeman/bedrock
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=3
OnFailure=bedrock-rollback.service

[Service]
Type=notify
NotifyAccess=main
ExecStart=%s daemon run
Restart=always
RestartSec=2
WatchdogSec=30
StateDirectory=bedrock
StateDirectoryMode=0700
RuntimeDirectory=bedrock
RuntimeDirectoryMode=0750
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
`

// RollbackUnitPath is the unit systemd runs when the daemon can't start.
const RollbackUnitPath = "/etc/systemd/system/bedrock-rollback.service"

const rollbackUnit = `[Unit]
Description=Put the previous bedrock binary back after a failed start

[Service]
Type=oneshot
ExecStart=` + host.RollbackScript + `
`

// Install writes the units for this binary, enables the daemon, starts it,
// and waits for it to answer. It needs root and systemd.
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
	if err := os.MkdirAll(host.LibDir, 0o755); err != nil {
		return err
	}
	for path, content := range map[string]string{UnitPath: fmt.Sprintf(unitTemplate, exe), RollbackUnitPath: rollbackUnit} {
		if current, _ := os.ReadFile(path); string(current) != content {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "wrote %s\n", path)
		}
	}
	if current, _ := os.ReadFile(host.RollbackScript); string(current) != host.RollbackScriptContent {
		if err := os.WriteFile(host.RollbackScript, []byte(host.RollbackScriptContent), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n", host.RollbackScript)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "bedrock.service"}, {"restart", "bedrock.service"}} {
		if err := systemctl(ctx, args...); err != nil {
			return err
		}
	}
	client := api.Dial(DefaultSocket)
	defer client.Close()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if info, err := client.Version(ctx); err == nil {
			fmt.Fprintf(out, "bedrock daemon %s is running\n", info.Version)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon didn't answer on %s within 20s; see journalctl -u bedrock", DefaultSocket)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Status reports the unit's state and whether the daemon answers.
func Status(ctx context.Context, socket string) (string, error) {
	var b strings.Builder
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		outp, _ := exec.CommandContext(ctx, "systemctl", "show", "bedrock.service", "--property=ActiveState,SubState,NRestarts,MainPID").Output()
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
