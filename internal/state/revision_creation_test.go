package state

import (
	"context"
	"testing"
	"time"
)

func TestCreateRevisionCannotReplaceExistingAuthority(t *testing.T) {
	store := open(t)
	ctx := context.Background()
	original := Revision{App: "site", ID: "r1", Status: RevisionActive, Images: map[string]string{"web": "sha256:original"}, SecretsVersion: 3, CreatedAt: time.Now()}
	if err := store.CreateRevision(ctx, original); err != nil {
		t.Fatal(err)
	}
	replacement := original
	replacement.Status, replacement.Images, replacement.SecretsVersion = RevisionFailed, nil, 0
	if err := store.CreateRevision(ctx, replacement); err == nil {
		t.Fatal("creation replaced an existing revision")
	}
	got, err := store.GetRevision(ctx, "site", "r1")
	if err != nil || got.Status != original.Status || got.Images["web"] != original.Images["web"] || got.SecretsVersion != original.SecretsVersion {
		t.Fatalf("authority changed: %+v %v", got, err)
	}
}
