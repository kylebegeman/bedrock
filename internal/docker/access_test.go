package docker

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAUserCanWriteWhatItOwnsOrWhatItsGroupOrAnyoneMayWrite(t *testing.T) {
	u := User{UID: 10001, GID: 10001}
	cases := []struct {
		name     string
		uid, gid int
		mode     fs.FileMode
		want     bool
	}{
		{"owned", 10001, 10001, 0o644, true},
		{"owned and read-only, which the owner can change", 10001, 10001, 0o444, true},
		{"root's, as a hand copy leaves it", 0, 0, 0o644, false},
		{"root's, group-writable by the user's group", 0, 10001, 0o664, true},
		{"root's, in the user's group but not group-writable", 0, 10001, 0o644, false},
		{"someone else's, writable by anyone", 70, 70, 0o666, true},
		{"someone else's directory, open to anyone", 70, 70, fs.ModeDir | 0o777, true},
		{"root's directory", 0, 0, fs.ModeDir | 0o755, false},
	}
	for _, c := range cases {
		if got := writableBy(u, c.uid, c.gid, c.mode); got != c.want {
			t.Errorf("%s: writable = %v, want %v", c.name, got, c.want)
		}
	}
	if !writableBy(User{}, 70, 70, 0o400) {
		t.Error("root can write anything")
	}
}

func TestAContainersUserIsKnownOnlyWhenNumeric(t *testing.T) {
	for spec, want := range map[string]User{"10001:10001": {UID: 10001, GID: 10001}, "65534": {UID: 65534, GID: 65534}, " 0:0 ": {}} {
		if got, ok := ParseUser(spec); !ok || got != want {
			t.Errorf("%q = %+v, %v", spec, got, ok)
		}
	}
	for _, spec := range []string{"", "node", "1000:staff", "-1"} {
		if _, ok := ParseUser(spec); ok {
			t.Errorf("%q should not be known", spec)
		}
	}
}

// someoneElse is a user that is not the one running the test and shares
// none of its groups, so every file the test makes is someone else's.
func someoneElse() User {
	return User{UID: os.Getuid() + 1, GID: os.Getgid() + 1}
}

func TestAScanNamesTheFilesAUserCantWriteAndCountsThemAll(t *testing.T) {
	root := t.TempDir()
	write := func(name string, mode fs.FileMode) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil { // past the umask
			t.Fatal(err)
		}
	}
	write("app.sqlite", 0o644)
	write("app.sqlite-wal", 0o644)
	write("shared/notes.txt", 0o666)
	write("shared/more/a", 0o644)
	if err := os.Symlink("app.sqlite", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}

	got, err := UnwritableBy(root, someoneElse())
	if err != nil {
		t.Fatal(err)
	}
	// The root is open to anyone; notes.txt is too. The two sqlite files,
	// shared, shared/more and shared/more/a are not; the link is skipped.
	if got.Count != 5 || got.Partial {
		t.Fatalf("count %d partial %v, want 5 of everything: %+v", got.Count, got.Partial, got.Examples)
	}
	if len(got.Examples) != 3 || got.Examples[0].Path != "app.sqlite" || got.Examples[0].Mode.Perm() != 0o644 {
		t.Fatalf("examples: %+v", got.Examples)
	}
	if got.Examples[0].UID != os.Getuid() {
		t.Fatalf("the example names its owner: %+v", got.Examples[0])
	}

	mine, err := UnwritableBy(root, User{UID: os.Getuid(), GID: os.Getgid()})
	if err != nil || mine.Count != 0 {
		t.Fatalf("the owner can write it all: %+v, %v", mine, err)
	}
	if asRoot, err := UnwritableBy(root, User{}); err != nil || asRoot.Count != 0 {
		t.Fatalf("root can write it all: %+v, %v", asRoot, err)
	}
	if _, err := UnwritableBy(filepath.Join(root, "missing"), someoneElse()); err == nil {
		t.Fatal("a missing volume is an error, not a clean bill")
	}
}

func TestTheProblemReadsLikeWhatWentWrongOnTheMachine(t *testing.T) {
	x := Unwritable{Count: 3, Examples: []Entry{
		{Path: "begamin.sqlite", UID: 0, GID: 0, Mode: 0o644},
		{Path: "begamin.sqlite-shm", UID: 0, GID: 0, Mode: 0o644},
		{Path: "begamin.sqlite-wal", UID: 0, GID: 0, Mode: 0o644},
	}}
	got := x.Problem("web", User{UID: 10001, GID: 10001}, "data")
	want := "web runs as 10001:10001 and can't write 3 files or directories in volume data, such as begamin.sqlite (0:0, 644), begamin.sqlite-shm (0:0, 644), begamin.sqlite-wal (0:0, 644)"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	one := Unwritable{Count: 1, Examples: x.Examples[:1]}.Problem("web", User{UID: 10001, GID: 10001}, "data")
	if !strings.Contains(one, "can't write 1 file or directory in volume data") {
		t.Fatalf("one: %q", one)
	}
	partial := Unwritable{Count: 9, Partial: true, Examples: x.Examples[:1]}.Problem("web", User{UID: 10001, GID: 10001}, "data")
	if !strings.Contains(partial, "at least 9 files") {
		t.Fatalf("partial: %q", partial)
	}
	if (Unwritable{}).Problem("web", User{UID: 1}, "data") != "" {
		t.Fatal("nothing wrong says nothing")
	}
}
