package cli

import (
	"testing"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

// The name is what the app is called forever after, so it has to come out
// of ordinary repository URLs correctly and always be one a manifest takes.
func TestTheAppNameComesOutOfTheRepositoryURL(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/kylebegeman/dragon-writer":     "dragon-writer",
		"https://github.com/kylebegeman/dragon-writer.git": "dragon-writer",
		"https://github.com/kylebegeman/dragon-writer/":    "dragon-writer",
		"git@github.com:kylebegeman/begamin.git":           "begamin",
		"ssh://git@example.com:2222/team/My_Site.git":      "my-site",
		"file:///srv/repos/notes.git":                      "notes",
		"https://github.com/you/KyleBegeman.com":           "kylebegeman-com",
	} {
		got := appNameFromRepo(repo)
		if got != want {
			t.Fatalf("%s: got %q, want %q", repo, got, want)
		}
		// Whatever comes out has to be usable as an app name.
		if err := (manifest.Scaffold{App: got, Kind: manifest.Worker}).Check(); err != nil {
			t.Fatalf("%s produced %q, which a manifest refuses: %v", repo, got, err)
		}
	}
}
