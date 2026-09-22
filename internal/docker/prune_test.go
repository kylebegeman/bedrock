package docker

import "testing"

// Docker reports what a prune reclaimed in whatever unit suits the number,
// and gc turns that into the line a person reads. A unit it cannot read
// must count as nothing rather than as a bare number of bytes, which would
// report a gigabyte reclaim as "1.2 B".
func TestParseReclaimed(t *testing.T) {
	for _, c := range []struct {
		line string
		want int64
	}{
		{"Total:\t0B", 0},
		{"Total:\t49.25GB", 49_250_000_000},
		{"Total reclaimed space: 0B", 0},
		{"Total reclaimed space: 512KB", 512_000},
		{"Total reclaimed space: 1.5MB", 1_500_000},
		{"Total reclaimed space: 49.25GB", 49_250_000_000},
		{"Total reclaimed space: 2TB", 2_000_000_000_000},
		{"Deleted build cache objects:", 0},
		{"", 0},
	} {
		if got := parseReclaimed(c.line); got != c.want {
			t.Errorf("parseReclaimed(%q) = %d, want %d", c.line, got, c.want)
		}
	}
}
