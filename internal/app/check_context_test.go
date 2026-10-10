package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

type checkContainer struct{ info *docker.Info }

func (c checkContainer) Inspect(context.Context, string) (*docker.Info, error) { return c.info, nil }

func TestGuardedContentChecksRemainIndependentOfAnonymousEdgeDenial(t *testing.T) {
	application := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "site.example.com" || r.URL.RequestURI() != "/posts?limit=2" {
			t.Errorf("application received %s %s", r.Host, r.URL.RequestURI())
		}
		io.WriteString(w, "two posts")
	}))
	defer application.Close()
	u, _ := url.Parse(application.URL)
	port, _ := strconv.Atoi(u.Port())
	m := &manifest.Manifest{App: "site", Workloads: map[string]manifest.Workload{"web": {Kind: manifest.Web, Port: port, Routes: []manifest.Route{{Host: "site.example.com", Auth: manifest.AuthLoom}}}}, Checks: []manifest.Check{{URL: "https://site.example.com/posts?limit=2", Contains: "two posts"}}}
	checks := verificationChecks(m)
	if len(checks.application) != 1 || len(checks.edge) != 0 || len(checks.guards) != 1 {
		t.Fatalf("wrong contexts: %+v", checks)
	}
	e := checkContainer{info: &docker.Info{IPs: map[string]string{docker.EdgeNetwork: u.Hostname()}}}
	if err := runCheckDirect(context.Background(), e, m, map[string]string{"web": "container"}, checks.application[0]); err != nil {
		t.Fatal(err)
	}
	checks.application[0].Contains = "missing post"
	if err := runCheckDirect(context.Background(), e, m, map[string]string{"web": "container"}, checks.application[0]); err == nil {
		t.Fatal("a protected route hid an application content failure")
	}
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Fatal("anonymous guard carried credentials")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer guard.Close()
	if err := guardCheck(context.Background(), manifest.Check{URL: guard.URL}, guard.Client().Transport); err != nil {
		t.Fatal(err)
	}
}

func TestCheckContextsPreserveLegacyChecksAndAlwaysTestGuards(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		auth                 manifest.AuthMode
		context              manifest.CheckContext
		direct, edge, guards int
	}{
		{"legacy", "", "", 1, 1, 0}, {"application", "", manifest.CheckApplication, 1, 0, 0}, {"edge", "", manifest.CheckEdge, 0, 1, 0},
		{"guarded", manifest.AuthLoom, "", 1, 0, 1}, {"guarded application", manifest.AuthLoom, manifest.CheckApplication, 1, 0, 1}, {"guarded authenticated edge", manifest.AuthLoom, manifest.CheckEdge, 0, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &manifest.Manifest{Workloads: map[string]manifest.Workload{"web": {Routes: []manifest.Route{{Host: "site.example.com", Auth: tc.auth}}}}, Checks: []manifest.Check{{URL: "https://site.example.com/", Context: tc.context}}}
			got := verificationChecks(m)
			if len(got.application) != tc.direct || len(got.edge) != tc.edge || len(got.guards) != tc.guards {
				t.Fatalf("contexts %+v", got)
			}
		})
	}
}

func TestGuardProbeDoesNotFollowSignInAndRejectsOpenOrBrokenRoutes(t *testing.T) {
	for _, status := range []int{200, 401, 403, 302, 404, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			loginRequests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/login" {
					loginRequests++
					io.WriteString(w, "login")
					return
				}
				if status == 302 {
					w.Header().Set("Location", "/login")
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()
			err := guardCheck(context.Background(), manifest.Check{URL: srv.URL}, srv.Client().Transport)
			wantOK := status == 401 || status == 403 || status == 302
			if (err == nil) != wantOK {
				t.Fatalf("guard error %v", err)
			}
			if loginRequests != 0 {
				t.Fatal("followed redirect to the sign-in page")
			}
		})
	}
}

func TestCredentialedEdgeCheckUsesNamedSecretsWithoutExposingThem(t *testing.T) {
	const cookie = "session=private-session-value"
	const authorization = "Bearer private-token-value"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != cookie || r.Header.Get("Authorization") != authorization {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/protected", http.StatusFound)
			return
		}
		io.WriteString(w, "protected content")
	}))
	defer srv.Close()
	check := manifest.Check{URL: srv.URL + "/start", Context: manifest.CheckEdge, Contains: "protected content", HeadersFromSecrets: map[string]string{"Cookie": "CHECK_COOKIE", "Authorization": "CHECK_AUTHORIZATION"}}
	values := map[string]string{"CHECK_COOKIE": cookie, "CHECK_AUTHORIZATION": authorization}
	if err := credentialCheck(context.Background(), check, values, srv.Client().Transport); err != nil {
		t.Fatal(err)
	}
	if err := guardCheck(context.Background(), manifest.Check{URL: srv.URL + "/protected"}, srv.Client().Transport); err != nil {
		t.Fatal(err)
	}
	if err := credentialCheck(context.Background(), check, nil, srv.Client().Transport); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("missing credential: %v", err)
	}
	values["CHECK_COOKIE"] = "bad\r\nCookie: injected"
	if err := credentialCheck(context.Background(), check, values, srv.Client().Transport); err == nil || strings.Contains(err.Error(), "injected") {
		t.Fatalf("unsafe header: %v", err)
	}
}

func TestCredentialedCheckNeverForwardsSecretsToAnotherOrigin(t *testing.T) {
	const value = "credential-never-forwarded"
	otherRequests := 0
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests++
		io.WriteString(w, "should never arrive")
	}))
	defer other.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", other.URL+"/?reflected="+value)
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	c := manifest.Check{URL: origin.URL, Context: manifest.CheckEdge, HeadersFromSecrets: map[string]string{"X-Check-Credential": "CHECK_TOKEN"}}
	err := credentialCheck(context.Background(), c, map[string]string{"CHECK_TOKEN": value}, origin.Client().Transport)
	if err == nil || !strings.Contains(err.Error(), "another origin") || strings.Contains(err.Error(), value) {
		t.Fatalf("redirect result: %v", err)
	}
	if otherRequests != 0 {
		t.Fatal("credential request reached another origin")
	}
}

func TestGuardedDeployPlansVerificationEvenWithoutContentChecks(t *testing.T) {
	m := &manifest.Manifest{App: "site", Workloads: map[string]manifest.Workload{"web": {Kind: manifest.Web, Image: "image", Port: 80, Routes: []manifest.Route{{Host: "site.example.com", Auth: manifest.AuthLoom}}}}}
	d := Deploy{Store: goldenStore(t), StateDir: t.TempDir()}
	plan, err := d.rollout(context.Background(), m, "20261010-120000", &buildFrom{source: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range plan.Steps {
		if st.Name == "verify" {
			found = true
			if !strings.Contains(st.Change, "1 sign-in guard") {
				t.Fatal(st.Change)
			}
		}
	}
	if !found {
		t.Fatal("guarded deploy did not verify anonymous denial")
	}
	m.Checks = []manifest.Check{{URL: "https://site.example.com/", Context: manifest.CheckEdge, HeadersFromSecrets: map[string]string{"Cookie": "CHECK_COOKIE"}}}
	if err := checkVerificationSecrets(m, map[string]string{}); err == nil || !strings.Contains(err.Error(), "CHECK_COOKIE") {
		t.Fatalf("missing credential preflight: %v", err)
	}
	if got := fmt.Sprint(CheckSecretNames(m)); got != "[CHECK_COOKIE]" {
		t.Fatal(got)
	}
}
