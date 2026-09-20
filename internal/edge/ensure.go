package edge

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
)

// The edge's container, image and the places it shares with this machine.
const (
	Container = "bedrock-edge"
	Image     = "caddy:2-alpine"
	// ConfigDir holds what the edge shares with the machine.
	ConfigDir = "/var/lib/bedrock/edge"
	// RunDir holds the sockets: Caddy's admin API, and bedrock's hooks
	// endpoint the edge proxies to. The edge sees it as /run/bedrock.
	RunDir = ConfigDir + "/run"
	// BootDir holds the configuration the edge starts with; the edge sees
	// it read-only as /etc/caddy/bedrock.
	BootDir = ConfigDir + "/boot"
	// AdminSocket is the admin API's socket, as this machine sees it.
	AdminSocket = RunDir + "/caddy.sock"
	// HooksSocket is where bedrock answers hooks, as this machine sees it.
	HooksSocket = RunDir + "/hooks.sock"
	// BootFile is the configuration the edge starts with.
	BootFile = BootDir + "/caddy.json"

	// Inside the edge.
	runDirInside  = "/run/bedrock"
	bootDirInside = "/etc/caddy/bedrock"
	adminListen   = "unix/" + runDirInside + "/caddy.sock"
	// HooksDial is how the edge reaches bedrock's hooks endpoint.
	HooksDial = "unix/" + runDirInside + "/hooks.sock"

	// LayoutLabel marks how the edge's container is made. A container
	// with another layout is replaced by Ensure.
	LayoutLabel = "bedrock.edge.layout"
	Layout      = "2"

	// AppNetworkPrefix starts the name of each app's edge network. The
	// dots keep it from colliding with any app's own network, which is
	// bedrock-<app> with hyphens only.
	AppNetworkPrefix = "bedrock.edge."
)

// AppNetwork names the network an app's serving workloads share with the
// edge and nothing else, so apps can't reach each other through it.
func AppNetwork(app string) string { return AppNetworkPrefix + app }

// Join puts the edge on an app's edge network.
func Join(ctx context.Context, engine *docker.Engine, app string) error {
	if err := engine.EnsureNetwork(ctx, AppNetwork(app)); err != nil {
		return err
	}
	return engine.ConnectNetwork(ctx, Container, AppNetwork(app))
}

// Leave takes the edge off an app's edge network and removes the network.
func Leave(ctx context.Context, engine *docker.Engine, app string) error {
	if err := engine.DisconnectNetwork(ctx, Container, AppNetwork(app)); err != nil {
		return err
	}
	return engine.RemoveNetwork(ctx, AppNetwork(app))
}

// Ensure runs the edge on ports 80 and 443 and waits until its admin API
// answers. It starts with boot, the whole configuration for the apps this
// machine runs, when it has to make the container; an edge made by an
// older bedrock is replaced that way, which takes a few seconds. Safe to
// call any time.
func Ensure(ctx context.Context, engine *docker.Engine, boot []byte, out io.Writer) error {
	if err := engine.EnsureNetwork(ctx, docker.EdgeNetwork); err != nil {
		return err
	}
	for _, dir := range []string{RunDir, BootDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if boot == nil {
		boot = Initial()
	}
	info, err := engine.Inspect(ctx, Container)
	switch {
	case err == nil && info.Labels[LayoutLabel] != Layout:
		// Made by an older bedrock, whose admin API listened on the network.
		if err := WriteBoot(BootFile, boot); err != nil {
			return err
		}
		if err := engine.Remove(ctx, Container, 10*time.Second); err != nil {
			return err
		}
		fmt.Fprintln(out, "the edge is replaced by one whose admin API only this machine reaches")
	case err != nil:
		if err := WriteBoot(BootFile, boot); err != nil {
			return err
		}
	default:
		if _, statErr := os.Stat(BootFile); statErr != nil {
			if err := WriteBoot(BootFile, boot); err != nil {
				return err
			}
		}
	}
	if !engine.HasImage(ctx, Image) {
		if err := engine.Pull(ctx, Image, out); err != nil {
			return err
		}
	}
	err = engine.Run(ctx, docker.Spec{
		Name:     Container,
		Image:    Image,
		Cmd:      []string{"caddy", "run", "--config", filepath.Join(bootDirInside, filepath.Base(BootFile))},
		Labels:   map[string]string{docker.LabelApp: "edge", LayoutLabel: Layout},
		Networks: []string{docker.EdgeNetwork},
		Publish:  []string{"80:80/tcp", "443:443/tcp", "443:443/udp"},
		Mounts: []string{
			"bedrock-edge-data:/data", "bedrock-edge-config:/config",
			RunDir + ":" + runDirInside, BootDir + ":" + bootDirInside + ":ro",
		},
		Restart: true,
		// Root, to write its certificates, but with nothing beyond
		// binding the web ports, and a read-only root filesystem.
		Isolated:     true,
		Capabilities: []string{"NET_BIND_SERVICE"},
		ReadOnly:     true,
	})
	if err != nil {
		return fmt.Errorf("edge: %w", err)
	}
	nets, err := engine.Networks(ctx, AppNetworkPrefix)
	if err != nil {
		return err
	}
	for _, n := range nets {
		if err := engine.ConnectNetwork(ctx, Container, n); err != nil {
			return err
		}
	}
	admin := NewAdmin()
	deadline := time.Now().Add(30 * time.Second)
	for !admin.Answers(ctx) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the edge started but its admin API didn't answer on %s; see docker logs %s", AdminSocket, Container)
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
