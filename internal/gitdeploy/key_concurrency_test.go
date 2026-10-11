package gitdeploy

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentKeyPermissionChangesPreserveEveryApp(t *testing.T) {
	key, err := ParseKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".ssh", "authorized_keys")
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Go(func() {
			<-start
			if err := UpdateKeys(path, func(keys *KeyFile) error { _, err := keys.Allow(key, fmt.Sprintf("app-%d", i)); return err }); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	workers.Wait()
	keys, err := ReadKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.Keys) != 1 || len(keys.Keys[0].Apps) != 20 {
		t.Fatalf("concurrent permission changes lost apps: %+v", keys.Keys)
	}
}
