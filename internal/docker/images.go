package docker

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/client"
)

// Image is an image bedrock built.
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

// OwnedImages lists the images bedrock built.
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

// PruneBuildCache drops build cache older than age, then trims whatever
// remains down to keep bytes, evicting least-recently-used entries first.
//
// Age alone cannot bound this. A machine that rebuilds a large source tree
// several times in one day holds every one of those caches, all of them
// younger than any sensible age limit, so an age rule reclaims nothing
// exactly when there is most to reclaim. The ceiling is what actually holds
// the disk; the age pass just keeps stale entries from occupying it.
func PruneBuildCache(ctx context.Context, age time.Duration, keep int64) (int64, error) {
	reclaimed := int64(0)
	prune := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "docker", append([]string{"builder", "prune", "--force"}, args...)...)
		result, err := cmd.CombinedOutput()
		text := strings.TrimSpace(string(result))
		if err != nil {
			return fmt.Errorf("builder prune: %v: %s", err, text)
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "Total:") || strings.HasPrefix(line, "Total reclaimed space") {
				reclaimed += parseReclaimed(line)
			}
		}
		return nil
	}
	if err := prune("--filter", fmt.Sprintf("until=%dh", int(age.Hours()))); err != nil {
		return reclaimed, err
	}
	if keep > 0 {
		// Docker renamed --keep-storage to --reserved-space; a daemon old
		// enough to reject the new name still answers to the old one.
		if err := prune("--reserved-space", fmt.Sprintf("%d", keep)); err != nil {
			if err2 := prune("--keep-storage", fmt.Sprintf("%d", keep)); err2 != nil {
				return reclaimed, err
			}
		}
	}
	return reclaimed, nil
}

// parseReclaimed reads the byte count out of docker's "Total reclaimed
// space: 1.23GB" line. An unreadable line counts as nothing rather than
// failing a prune that already succeeded.
func parseReclaimed(line string) int64 {
	m := regexp.MustCompile(`([0-9.]+)\s*([KMGT]?B)`).FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	switch m[2] {
	case "KB":
		return int64(n * 1e3)
	case "MB":
		return int64(n * 1e6)
	case "GB":
		return int64(n * 1e9)
	case "TB":
		return int64(n * 1e12)
	}
	return int64(n)
}
