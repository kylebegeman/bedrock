// Package version reports which build of quark is running.
package version

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// number is set at release time:
//
//	go build -ldflags "-X github.com/kylebegeman/quark/internal/version.number=0.7.0"
var number = "0.7.0-dev"

// Info describes one build.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Current returns this build's Info. The commit comes from the module's VCS
// stamp, so a build from a checkout knows where it came from.
func Current() Info {
	info := Info{Version: number, Commit: "unknown", Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 12 {
				info.Commit = setting.Value[:12]
			}
		}
	}
	return info
}

func (i Info) String() string {
	return fmt.Sprintf("quark %s (%s, %s %s/%s)", i.Version, i.Commit, i.Go, i.OS, i.Arch)
}

// WriteJSON writes the Info as one JSON object followed by a newline.
func (i Info) WriteJSON(w io.Writer) error {
	return json.NewEncoder(w).Encode(i)
}
