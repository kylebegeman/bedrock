package version

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestCurrentDescribesThisBuild(t *testing.T) {
	info := Current()
	if info.Version == "" || info.Commit == "" {
		t.Fatalf("incomplete: %+v", info)
	}
	if info.Go != runtime.Version() || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("wrong runtime facts: %+v", info)
	}
	if s := info.String(); !strings.HasPrefix(s, "bedrock "+info.Version) {
		t.Fatalf("unexpected rendering %q", s)
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := Current().WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Info
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if back != Current() {
		t.Fatalf("got %+v, want %+v", back, Current())
	}
}
