package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const site = `app: kylebegeman
description: Kyle's site
owner: personal
workloads:
  site:
    kind: static
    dir: dist
    routes:
      - host: kylebegeman.com
      - host: www.kylebegeman.com
  bakery:
    kind: web
    image: local/the-battered-baker:demo
    port: 8383
    routes:
      - host: kylebegeman.com
        path: /thebatteredbaker
    health:
      path: /
    resources:
      memory: 512m
checks:
  - url: https://kylebegeman.com/
    contains: Kyle Begeman
  - url: https://kylebegeman.com/kyle-begeman.vcf
    contains: "BEGIN:VCARD"
`

func TestParsesASiteWithTwoWorkloads(t *testing.T) {
	m, err := Parse([]byte(site))
	if err != nil {
		t.Fatal(err)
	}
	if m.App != "kylebegeman" || len(m.Workloads) != 2 || len(m.Checks) != 2 {
		t.Fatalf("%+v", m)
	}
	if got := m.WorkloadNames(); strings.Join(got, ",") != "bakery,site" {
		t.Fatalf("names %v", got)
	}
	if got := m.Hosts(); strings.Join(got, ",") != "kylebegeman.com,www.kylebegeman.com" {
		t.Fatalf("hosts %v", got)
	}
	if m.Workloads["bakery"].Routes[0].NormalizedPath() != "/thebatteredbaker/" || m.Workloads["site"].Routes[0].NormalizedPath() != "/" {
		t.Fatal("paths")
	}
}

func TestLoadReadsQuarkYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := Load(dir); err != nil || m.App != "kylebegeman" {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("a directory without a manifest must fail")
	}
}

func TestValidationNamesTheProblem(t *testing.T) {
	cases := map[string]string{
		"app: Bad Name\nworkloads:\n  w:\n    kind: worker\n    image: x\n": `app: "Bad Name" must be lowercase`,
		"app: a\nworkloads: {}\n":                                                                                                                   "workloads: an app needs at least one",
		"app: a\nworkloads:\n  w:\n    image: x\n":                                                                                                  "kind is required",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n":                                                                     "needs at least one route",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    routes: [{host: a.com}]\n":                                                      "needs port",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    build: {}\n    port: 80\n    routes: [{host: a.com}]\n":                         "give either image or build",
		"app: a\nworkloads:\n  w:\n    kind: static\n    routes: [{host: a.com}]\n":                                                                 "needs dir",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    routes: [{host: a.com}]\n":                                                   "a worker has no routes",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: not a host}]\n":                                   `isn't a hostname`,
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: a.com, path: nope}]\n":                            "must start with /",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: a.com}]\n    env: {bad: 1}\n":                     "must be an UPPER_CASE name",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: a.com}]\n    env: {KEY: 1}\n    secrets: [KEY]\n": "both a secret and a plain env value",
		"app: a\nworkloads:\n  w:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: a.com}]\n    resources: {memory: lots}\n":         "must look like 512m or 2g",
		"app: a\nworkloads:\n  w:\n    kind: static\n    dir: ../etc\n    routes: [{host: a.com}]\n":                                                "must be inside the source",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\nchecks:\n  - url: ftp://x\n":                                                     "url must start with https://",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    colour: blue\n":                                                              "field colour not found",
	}
	for input, want := range cases {
		_, err := Parse([]byte(input))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("input:\n%s\nwant error containing %q, got %v", input, want, err)
		}
	}
}

func TestTwoWorkloadsCannotClaimTheSameRoute(t *testing.T) {
	input := "app: a\nworkloads:\n  one:\n    kind: web\n    image: x\n    port: 80\n    routes: [{host: a.com}]\n  two:\n    kind: web\n    image: y\n    port: 80\n    routes: [{host: a.com, path: /}]\n"
	_, err := Parse([]byte(input))
	if err == nil || !strings.Contains(err.Error(), "a.com / is already routed to one") {
		t.Fatalf("got %v", err)
	}
}

func TestWorkloadForPicksTheLongestPrefix(t *testing.T) {
	m, err := Parse([]byte(site))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"/": "site", "/about": "site", "/thebatteredbaker": "bakery", "/thebatteredbaker/": "bakery", "/thebatteredbaker/menu": "bakery", "/thebatteredbakery": "site"}
	for path, want := range cases {
		name, _, ok := m.WorkloadFor("kylebegeman.com", path)
		if !ok || name != want {
			t.Errorf("%s: got %q (%v), want %q", path, name, ok, want)
		}
	}
	if _, _, ok := m.WorkloadFor("other.com", "/"); ok {
		t.Fatal("an unrouted host must not match")
	}
}
