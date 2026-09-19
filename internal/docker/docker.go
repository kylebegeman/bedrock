// Package docker is quark's view of the Docker Engine: containers,
// networks and images it owns, named readably and labeled so nothing else
// is ever touched. Builds go through the docker CLI, everything else
// through the Engine API.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Labels quark puts on everything it creates.
const (
	LabelOwner    = "quark.owner"
	LabelApp      = "quark.app"
	LabelWorkload = "quark.workload"
	LabelRevision = "quark.revision"
	OwnerValue    = "quark"
)

// EdgeNetwork is the network the edge and every routed workload share.
const EdgeNetwork = "quark-edge"

// Engine is a connection to the local Docker daemon.
type Engine struct {
	cli *client.Client
}

// Connect opens the connection. It fails only when Docker isn't there.
func Connect(ctx context.Context) (*Engine, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker isn't answering: %w", err)
	}
	return &Engine{cli: cli}, nil
}

// Close releases the connection.
func (e *Engine) Close() error { return e.cli.Close() }

// AppNetwork names an app's own network.
func AppNetwork(app string) string { return "quark-" + app }

// ContainerName names a workload's container for one revision.
func ContainerName(app, workload, revision string) string {
	return "quark-" + app + "-" + workload + "-" + revision
}

// EnsureNetwork creates a bridge network if it doesn't exist.
func (e *Engine) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := e.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		return nil
	}
	_, err := e.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{LabelOwner: OwnerValue},
	})
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// ConnectNetwork joins a container to a network. Already joined is fine.
func (e *Engine) ConnectNetwork(ctx context.Context, containerName, networkName string) error {
	_, err := e.cli.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{Container: containerName, EndpointConfig: &network.EndpointSettings{}})
	if err != nil && !strings.Contains(err.Error(), "already exists") && !errdefs.IsConflict(err) {
		return fmt.Errorf("connect %s to %s: %w", containerName, networkName, err)
	}
	return nil
}

// DisconnectNetwork takes a container off a network. Not joined, or no
// such network, is fine.
func (e *Engine) DisconnectNetwork(ctx context.Context, containerName, networkName string) error {
	_, err := e.cli.NetworkDisconnect(ctx, networkName, client.NetworkDisconnectOptions{Container: containerName, Force: true})
	if err != nil && !IsNotFound(err) && !strings.Contains(err.Error(), "is not connected") {
		return fmt.Errorf("disconnect %s from %s: %w", containerName, networkName, err)
	}
	return nil
}

// Networks lists the networks quark made whose names start with prefix.
func (e *Engine) Networks(ctx context.Context, prefix string) ([]string, error) {
	res, err := e.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range res.Items {
		if n.Labels[LabelOwner] == OwnerValue && strings.HasPrefix(n.Name, prefix) {
			out = append(out, n.Name)
		}
	}
	return out, nil
}

// Spec is a container quark wants running.
type Spec struct {
	Name   string
	Image  string
	Cmd    []string
	Env    []string
	Labels map[string]string
	// Networks the container joins; the first is its primary.
	Networks []string
	// Publish maps host ports, as "127.0.0.1:2019:2019/tcp" or "443:443/udp".
	Publish []string
	// Mounts as "volume:/path" or "/host/path:/path[:ro]".
	Mounts      []string
	MemoryBytes int64
	NanoCPUs    int64
	// Restart keeps the container running across reboots.
	Restart bool
	// HostNetwork puts the container on the machine's own network, for
	// helpers that only make outbound connections.
	HostNetwork bool
	// User overrides the image's user.
	User string
	// Isolated drops every capability and forbids gaining privileges,
	// except the ones in Capabilities. Privileged does the opposite.
	Isolated     bool
	Capabilities []string
	Privileged   bool
	// ReadOnly makes the root filesystem read-only; Tmpfs are writable
	// in-memory paths (/tmp comes with ReadOnly).
	ReadOnly bool
	Tmpfs    []string
	// PidsLimit bounds the processes in the container; 0 means the default.
	PidsLimit int64
	// Aliases are more names the container answers to on its first network.
	Aliases []string
	// Stdin, when set, is copied to the container's standard input, which
	// is closed at its end.
	Stdin io.Reader
	// NoHealthcheck turns off the image's own HEALTHCHECK, for a container
	// whose health quark checks its own way.
	NoHealthcheck bool
}

// DefaultPidsLimit bounds every isolated container's processes.
const DefaultPidsLimit int64 = 4096

// Run creates and starts a container, or starts it if it already exists.
func (e *Engine) Run(ctx context.Context, spec Spec) error {
	if info, err := e.Inspect(ctx, spec.Name); err == nil {
		if info.Running {
			return nil
		}
		_, err := e.cli.ContainerStart(ctx, info.ID, client.ContainerStartOptions{})
		return err
	}
	labels := map[string]string{LabelOwner: OwnerValue}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	hostConfig := &container.HostConfig{
		Binds:          spec.Mounts,
		LogConfig:      container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "20m", "max-file": "5"}},
		Resources:      container.Resources{Memory: spec.MemoryBytes, NanoCPUs: spec.NanoCPUs},
		ReadonlyRootfs: spec.ReadOnly,
		Privileged:     spec.Privileged,
	}
	if !spec.Privileged {
		hostConfig.SecurityOpt = []string{"no-new-privileges"}
	}
	if spec.Isolated && !spec.Privileged {
		hostConfig.CapDrop = []string{"ALL"}
		hostConfig.CapAdd = spec.Capabilities
		pids := spec.PidsLimit
		if pids == 0 {
			pids = DefaultPidsLimit
		}
		hostConfig.Resources.PidsLimit = &pids
	} else if spec.PidsLimit > 0 {
		// A privileged container keeps a bound it asked for.
		pids := spec.PidsLimit
		hostConfig.Resources.PidsLimit = &pids
	}
	if spec.ReadOnly || len(spec.Tmpfs) > 0 {
		hostConfig.Tmpfs = map[string]string{}
		if spec.ReadOnly {
			hostConfig.Tmpfs["/tmp"] = "rw,nosuid"
		}
		for _, t := range spec.Tmpfs {
			hostConfig.Tmpfs[t] = "rw,nosuid"
		}
	}
	if spec.Restart {
		hostConfig.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	}
	if spec.HostNetwork {
		hostConfig.NetworkMode = "host"
	}
	exposed := network.PortSet{}
	if len(spec.Publish) > 0 {
		hostConfig.PortBindings = network.PortMap{}
		for _, p := range spec.Publish {
			port, binding, err := parsePublish(p)
			if err != nil {
				return err
			}
			exposed[port] = struct{}{}
			hostConfig.PortBindings[port] = append(hostConfig.PortBindings[port], binding)
		}
	}
	endpoints := map[string]*network.EndpointSettings{}
	if len(spec.Networks) > 0 {
		endpoints[spec.Networks[0]] = &network.EndpointSettings{Aliases: spec.Aliases}
	}
	config := &container.Config{Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, Labels: labels, ExposedPorts: exposed, User: spec.User}
	if spec.NoHealthcheck {
		config.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
	}
	if spec.Stdin != nil {
		config.OpenStdin, config.StdinOnce, config.AttachStdin = true, true, true
	}
	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             spec.Name,
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: endpoints},
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", spec.Name, err)
	}
	for _, extra := range spec.Networks[min(1, len(spec.Networks)):] {
		if _, err := e.cli.NetworkConnect(ctx, extra, client.NetworkConnectOptions{Container: created.ID, EndpointConfig: &network.EndpointSettings{}}); err != nil {
			return fmt.Errorf("connect %s to %s: %w", spec.Name, extra, err)
		}
	}
	if spec.Stdin != nil {
		// Attached before the start, so no input is lost.
		attached, err := e.cli.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true})
		if err != nil {
			return fmt.Errorf("attach %s: %w", spec.Name, err)
		}
		if _, err := e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			attached.Close()
			return fmt.Errorf("start %s: %w", spec.Name, err)
		}
		go func() {
			defer attached.Close()
			_, _ = io.Copy(attached.Conn, spec.Stdin)
			_ = attached.CloseWrite()
			// Hold the connection until the container has read what it
			// wants; its end closes the stream.
			_, _ = io.Copy(io.Discard, attached.Reader)
		}()
		return nil
	}
	if _, err := e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", spec.Name, err)
	}
	return nil
}

func parsePublish(spec string) (network.Port, network.PortBinding, error) {
	proto := "tcp"
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		proto = spec[i+1:]
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	var hostIP, hostPort, containerPort string
	switch len(parts) {
	case 2:
		hostPort, containerPort = parts[0], parts[1]
	case 3:
		hostIP, hostPort, containerPort = parts[0], parts[1], parts[2]
	default:
		return network.Port{}, network.PortBinding{}, fmt.Errorf("bad port publish %q", spec)
	}
	port, err := network.ParsePort(containerPort + "/" + proto)
	if err != nil {
		return network.Port{}, network.PortBinding{}, err
	}
	binding := network.PortBinding{HostPort: hostPort}
	if hostIP != "" {
		addr, err := netip.ParseAddr(hostIP)
		if err != nil {
			return network.Port{}, network.PortBinding{}, fmt.Errorf("bad host address in %q: %w", spec, err)
		}
		binding.HostIP = addr
	}
	return port, binding, nil
}

// Info is what quark needs to know about a container.
type Info struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Status  string
	Health  string
	// Restarts counts the times Docker restarted it.
	Restarts   int
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	// IPs by network.
	IPs    map[string]string
	Labels map[string]string
}

// Inspect describes a container by name or ID.
func (e *Engine) Inspect(ctx context.Context, name string) (*Info, error) {
	res, err := e.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	c := res.Container
	info := &Info{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), IPs: map[string]string{}, Restarts: c.RestartCount}
	if c.Config != nil {
		info.Image, info.Labels = c.Config.Image, c.Config.Labels
	}
	if c.State != nil {
		info.Running, info.Status, info.ExitCode = c.State.Running, string(c.State.Status), c.State.ExitCode
		info.StartedAt, _ = time.Parse(time.RFC3339Nano, c.State.StartedAt)
		info.FinishedAt, _ = time.Parse(time.RFC3339Nano, c.State.FinishedAt)
		if c.State.Health != nil {
			info.Health = string(c.State.Health.Status)
		}
	}
	if c.NetworkSettings != nil {
		for net, ep := range c.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress.IsValid() {
				info.IPs[net] = ep.IPAddress.String()
			}
		}
	}
	return info, nil
}

// IsNotFound reports whether err means the thing doesn't exist.
func IsNotFound(err error) bool { return err != nil && errdefs.IsNotFound(err) }

// Remove stops (within timeout) and removes a container. A missing
// container is fine.
func (e *Engine) Remove(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if _, err := e.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &secs}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	// Anonymous volumes (an image's VOLUME lines) go with the container;
	// named volumes never do.
	if _, err := e.cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// Stop stops a container within timeout and keeps it, to start again.
// A missing container is fine.
func (e *Engine) Stop(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if _, err := e.cli.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &secs}); err != nil && !IsNotFound(err) {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	return nil
}

// Start starts a stopped container.
func (e *Engine) Start(ctx context.Context, name string) error {
	if _, err := e.cli.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// Owned lists every container quark created, running or not.
func (e *Engine) Owned(ctx context.Context) ([]Info, error) {
	res, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, c := range res.Items {
		if c.Labels[LabelOwner] != OwnerValue {
			continue
		}
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, Info{ID: c.ID, Name: name, Image: c.Image, Running: c.State == container.StateRunning, Status: c.Status, Labels: c.Labels})
	}
	return out, nil
}

// Unowned counts containers quark didn't create.
func (e *Engine) Unowned(ctx context.Context) ([]string, error) {
	res, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range res.Items {
		if c.Labels[LabelOwner] != OwnerValue && len(c.Names) > 0 {
			names = append(names, strings.TrimPrefix(c.Names[0], "/"))
		}
	}
	return names, nil
}

// Logs streams a container's output, stdout and stderr together.
func (e *Engine) Logs(ctx context.Context, name string, follow bool, tail string, w io.Writer) error {
	r, err := e.cli.ContainerLogs(ctx, name, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow, Tail: tail, Timestamps: false})
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = stdcopy.StdCopy(w, w, r)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// HasImage reports whether an image is present locally.
func (e *Engine) HasImage(ctx context.Context, ref string) bool {
	_, err := e.cli.ImageInspect(ctx, ref)
	return err == nil
}

// Pull fetches an image, writing progress lines to out.
func (e *Engine) Pull(ctx context.Context, ref string, out io.Writer) error {
	resp, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Close()
	_, err = io.Copy(io.Discard, resp)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	fmt.Fprintf(out, "pulled %s\n", ref)
	return nil
}

// Digest returns an image's repository digest reference, when it has one.
func (e *Engine) Digest(ctx context.Context, ref string) (string, error) {
	res, err := e.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	for _, d := range res.RepoDigests {
		if strings.HasPrefix(d, strings.SplitN(ref, ":", 2)[0]+"@") || strings.HasPrefix(d, repository(ref)+"@") {
			return d, nil
		}
	}
	if len(res.RepoDigests) > 0 {
		return res.RepoDigests[0], nil
	}
	return "", fmt.Errorf("%s has no repository digest", ref)
}

// repository strips the tag from an image reference.
func repository(ref string) string {
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

// RemoveImage deletes an image quark built. Missing is fine.
func (e *Engine) RemoveImage(ctx context.Context, ref string) error {
	_, err := e.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{})
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}
