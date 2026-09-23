package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return &Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "etc", "secrets.key")}
}

func TestKeyIsMadeOnceAndShownOnce(t *testing.T) {
	s := newStore(t)
	recipient, created, identity, err := s.EnsureKey()
	if err != nil || !created || !strings.HasPrefix(recipient, "age1") || !strings.HasPrefix(identity, "AGE-SECRET-KEY-1") {
		t.Fatalf("first: %v created=%v recipient=%q identity=%q", err, created, recipient, identity)
	}
	info, err := os.Stat(s.KeyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, info.Mode())
	}
	again, created, identity, err := s.EnsureKey()
	if err != nil || created || identity != "" || again != recipient {
		t.Fatalf("second: %v created=%v identity=%q recipient=%q", err, created, identity, again)
	}
}

func TestSetMakesVersionsAndRollbackCanReadOldOnes(t *testing.T) {
	s := newStore(t)
	if _, _, _, err := s.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Current("hello"); err != nil || v != 0 {
		t.Fatalf("no versions yet: %v %d", err, v)
	}
	v1, err := s.Set("hello", "SECRET_WORD", "alpha")
	if err != nil || v1 != 1 {
		t.Fatalf("set: %v %d", err, v1)
	}
	v2, err := s.Set("hello", "SECRET_WORD", "beta")
	if err != nil || v2 != 2 {
		t.Fatalf("set again: %v %d", err, v2)
	}
	v3, err := s.Set("hello", "OTHER", "x")
	if err != nil || v3 != 3 {
		t.Fatalf("set other: %v %d", err, v3)
	}
	old, err := s.Load("hello", 1)
	if err != nil || old["SECRET_WORD"] != "alpha" || len(old) != 1 {
		t.Fatalf("v1: %v %v", err, old)
	}
	now, v, err := s.LoadCurrent("hello")
	if err != nil || v != 3 || now["SECRET_WORD"] != "beta" || now["OTHER"] != "x" {
		t.Fatalf("current: %v %d %v", err, v, now)
	}
	names, cur, err := s.Names("hello")
	if err != nil || cur != 3 || strings.Join(names, ",") != "OTHER,SECRET_WORD" {
		t.Fatalf("names: %v %d %v", err, cur, names)
	}
	versions, err := s.Versions("hello")
	if err != nil || len(versions) != 3 || versions[0].Version != 3 || versions[2].Version != 1 || versions[2].Names[0] != "SECRET_WORD" {
		t.Fatalf("versions: %v %+v", err, versions)
	}
	if v4, err := s.Remove("hello", "OTHER"); err != nil || v4 != 4 {
		t.Fatalf("remove: %v %d", err, v4)
	}
	if _, err := s.Remove("hello", "OTHER"); err == nil {
		t.Fatal("removing a missing name must fail")
	}
	if _, err := s.Set("hello", "lower", "x"); err == nil {
		t.Fatal("lowercase names must be refused")
	}
	if got := Missing(now, []string{"SECRET_WORD", "MISSING"}); len(got) != 1 || got[0] != "MISSING" {
		t.Fatalf("missing: %v", got)
	}
}

func TestValuesAreSealedOnDisk(t *testing.T) {
	s := newStore(t)
	if _, _, _, err := s.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("hello", "SECRET_WORD", "hunter2-is-the-value"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "hello", "v1.age"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatal("the value is readable on disk")
	}
	meta, _ := os.ReadFile(filepath.Join(s.Dir, "hello", "v1.meta"))
	if strings.Contains(string(meta), "hunter2") || !strings.Contains(string(meta), "SECRET_WORD") {
		t.Fatalf("meta: %s", meta)
	}
	// Another machine's key can't read it.
	other := newStore(t)
	other.Dir = s.Dir
	if _, _, _, err := other.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Load("hello", 1); err == nil {
		t.Fatal("a different key must not decrypt the store")
	}
}

func TestConcurrentChangesLoseNothing(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "key")}
	if _, _, _, err := s.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Set("app", fmt.Sprintf("NAME_%d", i), "v"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	values, version, err := s.LoadCurrent("app")
	if err != nil || len(values) != 20 || version != 20 {
		t.Fatalf("%d names in version %d (%v): every change must build on the one before", len(values), version, err)
	}
}

func TestRemovingAnAppDropsEverySealedVersionAndSparesBedrocks(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "key")}
	if _, _, _, err := s.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"one", "two"} {
		if _, err := s.Set("hello", "TOKEN", v); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RemoveApp("hello"); err != nil {
		t.Fatal(err)
	}
	if versions, _ := s.Versions("hello"); len(versions) != 0 {
		t.Fatalf("versions survived: %+v", versions)
	}
	if current, _ := s.Current("hello"); current != 0 {
		t.Fatalf("current version %d after removal", current)
	}
	if _, err := os.Stat(s.appDir("hello")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the app's directory is still there: %v", err)
	}
	if err := s.RemoveApp("bedrock"); err == nil {
		t.Fatal("bedrock's own secrets were removable")
	}
	if err := s.RemoveApp("never-here"); err != nil {
		t.Fatalf("removing an app that has no secrets: %v", err)
	}
}
