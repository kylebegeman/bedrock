package edge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
)

// Container is the edge's container; Image is what it runs.
const (
	Container = "quark-edge"
	Image     = "caddy:2-alpine"
	// ConfigDir holds the initial configuration the edge boots with.
	ConfigDir = "/var/lib/quark/edge"
)

// Ensure runs the edge on ports 80 and 443 with its admin API on
// 127.0.0.1:2019, and waits until it answers. Safe to call any time.
func Ensure(ctx context.Context, engine *docker.Engine, out interface{ Write([]byte) (int, error) }) error {
	if err := engine.EnsureNetwork(ctx, docker.EdgeNetwork); err != nil {
		return err
	}
	if err := os.MkdirAll(ConfigDir, 0o755); err != nil {
		return err
	}
	initial := filepath.Join(ConfigDir, "initial.json")
	if _, err := os.Stat(initial); err != nil {
		if err := os.WriteFile(initial, Initial(), 0o644); err != nil {
			return err
		}
	}
	if !engine.HasImage(ctx, Image) {
		if err := engine.Pull(ctx, Image, out); err != nil {
			return err
		}
	}
	err := engine.Run(ctx, docker.Spec{
		Name:     Container,
		Image:    Image,
		Cmd:      []string{"caddy", "run", "--config", "/etc/caddy/initial.json", "--resume"},
		Labels:   map[string]string{docker.LabelApp: "edge"},
		Networks: []string{docker.EdgeNetwork},
		Publish:  []string{"80:80/tcp", "443:443/tcp", "443:443/udp", "127.0.0.1:2019:2019/tcp"},
		Mounts:   []string{"quark-edge-data:/data", "quark-edge-config:/config", initial + ":/etc/caddy/initial.json:ro"},
		Restart:  true,
	})
	if err != nil {
		return fmt.Errorf("edge: %w", err)
	}
	admin := NewAdmin()
	deadline := time.Now().Add(30 * time.Second)
	for !admin.Answers(ctx) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the edge started but its admin API didn't answer on %s", AdminAddress)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintln(out, "edge answering")
	return nil
}
