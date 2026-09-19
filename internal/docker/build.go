package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Registry is the local registry every build is pushed to, so images get
// digests and survive pruning.
const Registry = "127.0.0.1:5000"

// ImageRef names a workload's image for a revision in the local registry.
func ImageRef(app, workload, revision string) string {
	return fmt.Sprintf("%s/%s/%s:%s", Registry, app, workload, revision)
}

// BuildSpec is one image build.
type BuildSpec struct {
	Ref        string
	Context    string
	Dockerfile string
	Target     string
	Args       map[string]string
	Labels     map[string]string
}

// Build runs docker buildx at low priority, pushes the image to the local
// registry and returns its digest reference. Output streams to out.
func Build(ctx context.Context, spec BuildSpec, out io.Writer) error {
	args := []string{"-n", "19", "ionice", "-c", "3", "docker", "buildx", "build", "--load", "--progress=plain", "-t", spec.Ref}
	if spec.Dockerfile != "" {
		args = append(args, "-f", filepath.Join(spec.Context, spec.Dockerfile))
	}
	if spec.Target != "" {
		args = append(args, "--target", spec.Target)
	}
	keys := make([]string, 0, len(spec.Args))
	for k := range spec.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+spec.Args[k])
	}
	labelKeys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)
	for _, k := range labelKeys {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	args = append(args, spec.Context)
	if err := run(ctx, out, "nice", args...); err != nil {
		return fmt.Errorf("build %s: %w", spec.Ref, err)
	}
	if err := run(ctx, out, "docker", "push", "--quiet", spec.Ref); err != nil {
		return fmt.Errorf("push %s: %w", spec.Ref, err)
	}
	return nil
}

func run(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// StaticContext writes a build context that serves dir with Caddy's file
// server on port 8080, and returns its path. The caller removes it.
func StaticContext(sourceDir, dir string) (string, error) {
	tmp, err := os.MkdirTemp("", "quark-static-")
	if err != nil {
		return "", err
	}
	if err := copyTree(filepath.Join(sourceDir, dir), filepath.Join(tmp, "site")); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("static dir %s: %w", dir, err)
	}
	// Caddy's binary on plain Alpine: no VOLUME lines to leave anonymous
	// volumes behind, a user that isn't root, and state in /tmp so the
	// root filesystem can be read-only. The copy drops the binary's file
	// capability (to bind low ports, which 8080 isn't): with every
	// capability dropped, the kernel refuses to run a binary that has one.
	dockerfile := "FROM " + StaticServerImage + " AS caddy\n" +
		"RUN cp /usr/bin/caddy /caddy\n" +
		"FROM " + HelperImage + "\n" +
		"COPY --from=caddy /caddy /usr/bin/caddy\n" +
		"COPY site /srv\n" +
		"ENV XDG_CONFIG_HOME=/tmp/caddy XDG_DATA_HOME=/tmp/caddy\n" +
		"USER 65534:65534\n" +
		"CMD [\"caddy\", \"file-server\", \"--root\", \"/srv\", \"--listen\", \":" + StaticPort + "\"]\n"
	if err := os.WriteFile(filepath.Join(tmp, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// StaticServerImage serves static workloads; StaticPort is where.
const (
	StaticServerImage = "caddy:2-alpine"
	StaticPort        = "8080"
)

func copyTree(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", from)
	}
	return filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// EnsureBuildLog trims a build's noisy progress into what a person wants.
func TrimBuildLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	switch {
	case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, "sha256:"):
		return "", false
	}
	return line, true
}
