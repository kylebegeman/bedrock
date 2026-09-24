package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ScanLimit bounds how many entries one scan of a volume reads, so a
// volume of millions of files costs the same as a large one.
const ScanLimit = 500_000

// scanExamples is how many unwritable entries a scan keeps to name.
const scanExamples = 3

// Unwritable is what a user can't write in a directory tree: entries it
// doesn't own and has no group or world write on. An entry the user owns
// but has made read-only is left out, since the owner can change that.
type Unwritable struct {
	// Count is how many entries the user can't write.
	Count int `json:"count"`
	// Examples are the first few, to name in a message.
	Examples []Entry `json:"examples,omitempty"`
	// Partial is set when the scan stopped at ScanLimit entries.
	Partial bool `json:"partial,omitempty"`
}

// Entry is one file or directory, by its path under the scanned root.
type Entry struct {
	Path string      `json:"path"`
	UID  int         `json:"uid"`
	GID  int         `json:"gid"`
	Mode fs.FileMode `json:"mode"`
}

// String reads like begamin.sqlite (0:0, 644).
func (e Entry) String() string {
	return fmt.Sprintf("%s (%d:%d, %o)", e.Path, e.UID, e.GID, e.Mode.Perm())
}

// ParseUser reads a container's user as Docker keeps it: uid, or uid:gid,
// numeric. Bedrock always sets it that way; anything else, including an
// empty user, which means the image's, is not known here.
func ParseUser(spec string) (User, bool) {
	who, group, hasGroup := strings.Cut(strings.TrimSpace(spec), ":")
	uid, err := strconv.Atoi(who)
	if err != nil || uid < 0 {
		return User{}, false
	}
	gid := uid
	if hasGroup {
		if gid, err = strconv.Atoi(group); err != nil || gid < 0 {
			return User{}, false
		}
	}
	return User{UID: uid, GID: gid}, true
}

// writableBy says whether a user can write an entry with this owner and
// mode: it owns it, or its group may write it, or anyone may. Supplementary
// groups are not considered; a container rarely has any.
func writableBy(u User, uid, gid int, mode fs.FileMode) bool {
	perm := mode.Perm()
	switch {
	case u.Root(), uid == u.UID:
		return true
	case gid == u.GID && perm&0o020 != 0:
		return true
	default:
		return perm&0o002 != 0
	}
}

// UnwritableBy walks a directory tree, such as a volume's mountpoint on
// the machine, and reports the files and directories the user can't
// write. Symbolic links are skipped: their own mode means nothing. Root
// can write everything, so a root user needs no walk.
func UnwritableBy(root string, u User) (Unwritable, error) {
	var out Unwritable
	if u.Root() {
		return out, nil
	}
	seen := 0
	errStop := errors.New("scan limit")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if seen++; seen > ScanLimit {
			out.Partial = true
			return errStop
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no owner to read", path)
		}
		uid, gid := int(st.Uid), int(st.Gid)
		if writableBy(u, uid, gid, info.Mode()) {
			return nil
		}
		out.Count++
		if len(out.Examples) < scanExamples {
			rel, _ := filepath.Rel(root, path)
			out.Examples = append(out.Examples, Entry{Path: rel, UID: uid, GID: gid, Mode: info.Mode()})
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return out, err
	}
	return out, nil
}

// Problem says what a workload can't write in a volume, for an alert, a
// doctor line or a failed drill; empty when there is nothing wrong.
func (x Unwritable) Problem(workload string, u User, volume string) string {
	if x.Count == 0 {
		return ""
	}
	count := strconv.Itoa(x.Count)
	if x.Partial {
		count = "at least " + count
	}
	noun := "files or directories"
	if x.Count == 1 && !x.Partial {
		noun = "file or directory"
	}
	names := make([]string, len(x.Examples))
	for i, e := range x.Examples {
		names[i] = e.String()
	}
	return fmt.Sprintf("%s runs as %s and can't write %s %s in volume %s, such as %s",
		workload, u.Spec(), count, noun, volume, strings.Join(names, ", "))
}
