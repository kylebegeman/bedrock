package integration

import "testing"

func TestVerifyURLSplitsIntoWhatTheEdgeDials(t *testing.T) {
	for raw, want := range map[string]Verify{
		"https://loom.example.com/api/cloud/auth/verify": {Dial: "loom.example.com:443", Path: "/api/cloud/auth/verify", TLS: true, Host: "loom.example.com"},
		"http://127.0.0.1:4773/verify":                   {Dial: "127.0.0.1:4773", Path: "/verify", TLS: false, Host: "127.0.0.1"},
		"https://loom.example.com:8443/v/check?x=1":      {Dial: "loom.example.com:8443", Path: "/v/check?x=1", TLS: true, Host: "loom.example.com"},
	} {
		got, err := ParseVerifyURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if *got != want {
			t.Fatalf("%s: got %+v, want %+v", raw, *got, want)
		}
	}
}

// A URL the edge cannot dial is a guarded route that answers nothing, so it
// is refused when it is set rather than when someone visits the site.
func TestVerifyURLRefusesWhatCannotBeDialled(t *testing.T) {
	for _, raw := range []string{
		"", "loom.example.com/verify", "ftp://loom.example.com/verify",
		"https:///verify", "https://loom.example.com", "https://loom.example.com/",
	} {
		if _, err := ParseVerifyURL(raw); err == nil {
			t.Fatalf("%q must be refused", raw)
		}
	}
}
