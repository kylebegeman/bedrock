package docker

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// Image is an image quark built.
type Image struct {
	ID     string
	Tags   []string
	Labels map[string]string
	Size   int64
}

// Describe names an image the way a person reads it.
func (i Image) Describe() string {
	if len(i.Tags) > 0 {
		return i.Tags[0]
	}
	return strings.TrimPrefix(i.ID, "sha256:")[:12]
}

// OwnedImages lists the images quark built.
func (e *Engine) OwnedImages(ctx context.Context) ([]Image, error) {
	res, err := e.cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, img := range res.Items {
		if img.Labels[LabelOwner] != OwnerValue {
			continue
		}
		var tags []string
		for _, t := range img.RepoTags {
			if t != "<none>:<none>" {
				tags = append(tags, t)
			}
		}
		out = append(out, Image{ID: img.ID, Tags: tags, Labels: img.Labels, Size: img.Size})
	}
	return out, nil
}

// PruneBuildCache drops build cache older than age.
func PruneBuildCache(ctx context.Context, age time.Duration, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "builder", "prune", "--force", "--filter", fmt.Sprintf("until=%dh", int(age.Hours())))
	result, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(result))
	if err != nil {
		return fmt.Errorf("builder prune: %v: %s", err, text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Total reclaimed space") {
			fmt.Fprintln(out, strings.ToLower(line[:1])+line[1:])
		}
	}
	return nil
}
