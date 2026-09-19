package app

import (
	"context"
	"fmt"
	"io"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/manifest"
)

// DefaultMemory bounds a workload that doesn't say how much memory it may
// use, so one app can't starve the rest of the machine.
const DefaultMemory int64 = 2 << 30

// isolate decides who a workload runs as and what it may do, and writes
// that into its container spec. By default: its image's user, or nobody
// when the image would run as root; no capabilities beyond the ones the
// manifest lists; no gaining privileges; a read-only root filesystem with
// /tmp in memory; bounded processes and memory. A privileged workload
// gets none of the restrictions, because its manifest says it needs them,
// and runs as its image's user, root included.
func isolate(ctx context.Context, e *docker.Engine, spec *docker.Spec, w manifest.Workload, image string) (docker.User, error) {
	var (
		u   docker.User
		err error
	)
	if w.Privileged && w.User == "" {
		imageUser, ierr := e.ImageUser(ctx, image)
		if ierr != nil {
			return docker.User{}, ierr
		}
		u, err = e.ResolveUser(ctx, image, imageUser)
	} else {
		u, err = e.EffectiveUser(ctx, image, w.User)
	}
	if err != nil {
		return docker.User{}, err
	}
	spec.User = u.Spec()
	if spec.MemoryBytes == 0 {
		spec.MemoryBytes = DefaultMemory
	}
	if w.Privileged {
		spec.Privileged = true
		return u, nil
	}
	spec.Isolated = true
	spec.Capabilities = w.CapabilityNames()
	spec.ReadOnly = !w.WritableRoot
	spec.Tmpfs = w.Tmpfs
	return u, nil
}

// ownVolumes gives each volume a workload mounts to the user it runs as,
// when the volume still belongs to root: a fresh volume, or one unpacked
// from an archive.
func ownVolumes(ctx context.Context, e *docker.Engine, m *manifest.Manifest, w manifest.Workload, u docker.User, out io.Writer) error {
	for _, mt := range w.Mounts {
		changed, err := e.EnsureVolumeOwner(ctx, docker.VolumeName(m.App, mt.Volume), u)
		if err != nil {
			return err
		}
		if changed {
			fmt.Fprintf(out, "volume %s now belongs to %s\n", mt.Volume, u)
		}
	}
	return nil
}
