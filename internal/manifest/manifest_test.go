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

func TestLoadReadsBedrockYAML(t *testing.T) {
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
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    health: {command: [true], timeout: soon}\n":                                  `health.timeout: "soon" isn't a duration`,
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\nchecks:\n  - url: https://x\n    within: 5\n":                                    `within: "5" isn't a duration`,
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
		name, _, _, ok := m.RouteFor("kylebegeman.com", path)
		if !ok || name != want {
			t.Errorf("%s: got %q (%v), want %q", path, name, ok, want)
		}
	}
	if _, _, _, ok := m.RouteFor("other.com", "/"); ok {
		t.Fatal("an unrouted host must not match")
	}
}

const withData = `app: writer
workloads:
  web:
    kind: web
    image: x
    port: 3000
    routes: [{host: writer.example.com}]
    mounts:
      - volume: uploads
        path: /app/public/uploads
  backup:
    kind: cron
    image: x
    schedule: "0 * * * *"
    timeout: 30m
    command: [sh, /app/backup.sh]
    mounts:
      - volume: uploads
        path: /uploads
data:
  postgres:
    version: "16"
  volumes:
    uploads:
      description: Her drawings
`

func TestDataVolumesMountsAndCron(t *testing.T) {
	m, err := Parse([]byte(withData))
	if err != nil {
		t.Fatal(err)
	}
	if m.PostgresVersion() != "16" || len(m.Data.Volumes) != 1 || m.Workloads["web"].Mounts[0].Path != "/app/public/uploads" {
		t.Fatalf("%+v", m)
	}
	if !m.Workloads["web"].LongRunning() || m.Workloads["backup"].LongRunning() || !m.Workloads["web"].Serves() || m.Workloads["backup"].Serves() {
		t.Fatal("kinds")
	}
	if sched, err := ParseSchedule(m.Workloads["backup"].Schedule); err != nil || sched == nil {
		t.Fatalf("schedule: %v", err)
	}
	plain, _ := Parse([]byte("app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n"))
	if plain.PostgresVersion() != "" {
		t.Fatal("no database by default")
	}
	cases := map[string]string{
		"app: a\nworkloads:\n  w:\n    kind: cron\n    image: x\n":                                                                  "needs schedule",
		"app: a\nworkloads:\n  w:\n    kind: cron\n    image: x\n    schedule: nope\n":                                              "schedule:",
		"app: a\nworkloads:\n  w:\n    kind: cron\n    image: x\n    schedule: \"* * * * *\"\n    timeout: soon\n":                  "isn't a duration",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    schedule: \"* * * * *\"\n":                                   "for cron workloads",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    mounts: [{volume: nope, path: /x}]\n":                        "isn't declared under data.volumes",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\n    mounts: [{volume: v, path: x}]\ndata:\n  volumes: {v: {}}\n": "path must be absolute",
		"app: a\nworkloads:\n  w:\n    kind: worker\n    image: x\ndata:\n  postgres: {version: \"9\"}\n":                           "supported major version",
	}
	for input, want := range cases {
		if _, err := Parse([]byte(input)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("input:\n%s\nwant %q, got %v", input, want, err)
		}
	}
}
