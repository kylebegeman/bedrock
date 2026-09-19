package docker

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Exposure is what a running container may do and what reaches it, read
// from Docker rather than from any manifest.
type Exposure struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"-"`
	App     string            `json:"app,omitempty"`
	Running bool              `json:"running"`
	// User is the user the container runs as; empty means the image's.
	User string `json:"user"`
	// Privileged containers keep every capability and see the devices.
	Privileged bool `json:"privileged"`
	// Capabilities are those beyond an empty set when everything else is
	// dropped; AllCapabilities means Docker's defaults were left in place.
	Capabilities    []string `json:"capabilities,omitempty"`
	AllCapabilities bool     `json:"all_capabilities"`
	NoNewPrivileges bool     `json:"no_new_privileges"`
	ReadOnlyRoot    bool     `json:"read_only_root"`
	Tmpfs           []string `json:"tmpfs,omitempty"`
	// Networks the container is on; HostNetwork means the machine's own.
	Networks    []string `json:"networks,omitempty"`
	HostNetwork bool     `json:"host_network"`
	// Ports are the ports published on the machine, as address:port/proto.
	Ports []string `json:"ports,omitempty"`
	// Mounts are volume:path and host-path:path, with :ro when read-only.
	Mounts      []string `json:"mounts,omitempty"`
	MemoryBytes int64    `json:"memory_bytes"`
	PidsLimit   int64    `json:"pids_limit"`
}

// Exposures describes every container on the machine, quark's and not.
func (e *Engine) Exposures(ctx context.Context) ([]Exposure, error) {
	res, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var out []Exposure
	for _, c := range res.Items {
		x, err := e.exposure(ctx, c.ID)
		if IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *Engine) exposure(ctx context.Context, id string) (*Exposure, error) {
	res, err := e.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	c := res.Container
	x := &Exposure{Name: strings.TrimPrefix(c.Name, "/")}
	if c.State != nil {
		x.Running = c.State.Running
	}
	if c.Config != nil {
		x.User, x.Labels = c.Config.User, c.Config.Labels
		x.App = c.Config.Labels[LabelApp]
	}
	if hc := c.HostConfig; hc != nil {
		x.Privileged = hc.Privileged
		x.ReadOnlyRoot = hc.ReadonlyRootfs
		x.HostNetwork = hc.NetworkMode == "host"
		x.MemoryBytes = hc.Memory
		if hc.PidsLimit != nil {
			x.PidsLimit = *hc.PidsLimit
		}
		for _, o := range hc.SecurityOpt {
			if strings.HasPrefix(o, "no-new-privileges") && !strings.HasSuffix(o, "false") {
				x.NoNewPrivileges = true
			}
		}
		x.AllCapabilities = !dropsAll(hc)
		for _, cap := range hc.CapAdd {
			x.Capabilities = append(x.Capabilities, strings.TrimPrefix(strings.ToUpper(cap), "CAP_"))
		}
		for path := range hc.Tmpfs {
			x.Tmpfs = append(x.Tmpfs, path)
		}
		sort.Strings(x.Tmpfs)
		for port, bindings := range hc.PortBindings {
			for _, b := range bindings {
				host := "0.0.0.0"
				if b.HostIP.IsValid() {
					host = b.HostIP.String()
				}
				x.Ports = append(x.Ports, fmt.Sprintf("%s:%s/%s", host, b.HostPort, port.Proto()))
			}
		}
		sort.Strings(x.Ports)
	}
	for _, m := range c.Mounts {
		src := m.Name
		if src == "" {
			src = m.Source
		}
		entry := src + ":" + m.Destination
		if !m.RW {
			entry += ":ro"
		}
		x.Mounts = append(x.Mounts, entry)
	}
	sort.Strings(x.Mounts)
	if c.NetworkSettings != nil {
		for name := range c.NetworkSettings.Networks {
			x.Networks = append(x.Networks, name)
		}
		sort.Strings(x.Networks)
	}
	return x, nil
}

func dropsAll(hc *container.HostConfig) bool {
	for _, cap := range hc.CapDrop {
		if strings.EqualFold(cap, "ALL") {
			return true
		}
	}
	return false
}
