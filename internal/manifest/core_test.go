package manifest

import (
	"strings"
	"testing"
)

const core = `app: loom
workloads:
  core-api:
    kind: web
    image: local/loom:1
    port: 4773
    aliases: [mail-broker]
    grace: 60s
    resources: {memory: 2g, pids: 1024}
    routes:
      - host: core.example.com
        path: /api
      - host: runner.core.example.com
        port: 4774
  processor:
    kind: worker
    image: local/loom:1
    health:
      command: [sh, -ec, "true"]
      timeout: 120s
  schema:
    kind: release
    image: local/loom:1
    order: 20
    timeout: 5m
  objects-bootstrap:
    kind: release
    image: quay.io/minio/mc:1
    order: 10
data:
  postgres:
    version: "17"
    image: pgvector/pgvector@sha256:0000000000000000000000000000000000000000000000000000000000000000
    user: loom_cluster_admin
    database: loom
    init: deploy/postgres/init
    env: {POSTGRES_INITDB_ARGS: --auth-host=scram-sha-256}
    secrets: [LOOM_CORE_API_PASSWORD]
    database_url: false
  objects: {}
secrets:
  generate:
    LOOM_CORE_API_PASSWORD: hex:32
    LOOM_VAULT_KEY: base64:32
    MINIO_ROOT_USER: value:loom-root
    LOOM_RUNNER_ENROLLMENT_TOKEN: hex:32
  derive:
    LOOM_CORE_API_DATABASE_URL: "postgresql://loom_core_api:{LOOM_CORE_API_PASSWORD}@{postgres}:5432/loom"
    LOOM_CORE_RUNNER_PAIRING_TOKEN_SHA256: "sha256:{sha256:LOOM_RUNNER_ENROLLMENT_TOKEN}"
    LOOM_OBJECTS_URL: "http://{objects}:9000"
`

func TestACoreManifestParses(t *testing.T) {
	m, err := Parse([]byte(core))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.ReleaseWorkloads(), ","); got != "objects-bootstrap,schema" {
		t.Fatalf("release order %s", got)
	}
	api := m.Workloads["core-api"]
	if api.RoutePort(api.Routes[1]) != 4774 || api.RoutePort(api.Routes[0]) != 4773 || api.LongRunning() == false {
		t.Fatalf("%+v", api)
	}
	if m.Workloads["schema"].LongRunning() {
		t.Fatal("a release workload isn't long running")
	}
	if u, d := m.PostgresIdentity(); u != "loom_cluster_admin" || d != "loom" {
		t.Fatalf("%s %s", u, d)
	}
	if m.InjectsDatabaseURL() || !m.HasObjects() {
		t.Fatal("database_url: false and objects: {}")
	}
	plain, _ := Parse([]byte("app: shop\nworkloads:\n  web:\n    kind: worker\n    image: x\ndata:\n  postgres: {}\n"))
	if u, d := plain.PostgresIdentity(); u != "shop" || d != "shop" || !plain.InjectsDatabaseURL() {
		t.Fatalf("defaults: %s %s", u, d)
	}
}

func TestSecretsAreDerivedFromOthersAndHosts(t *testing.T) {
	m, err := Parse([]byte(core))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"LOOM_CORE_API_PASSWORD": "pw", "LOOM_RUNNER_ENROLLMENT_TOKEN": "abc"}
	got, err := Derive(m.Secrets.Derive, values, map[string]string{"postgres": "bedrock-loom-postgres", "objects": "bedrock-loom-objects"})
	if err != nil {
		t.Fatal(err)
	}
	if got["LOOM_CORE_API_DATABASE_URL"] != "postgresql://loom_core_api:pw@bedrock-loom-postgres:5432/loom" {
		t.Fatalf("%q", got["LOOM_CORE_API_DATABASE_URL"])
	}
	// sha256("abc")
	if got["LOOM_CORE_RUNNER_PAIRING_TOKEN_SHA256"] != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("%q", got["LOOM_CORE_RUNNER_PAIRING_TOKEN_SHA256"])
	}
	if got["LOOM_OBJECTS_URL"] != "http://bedrock-loom-objects:9000" {
		t.Fatalf("%q", got["LOOM_OBJECTS_URL"])
	}
	chain := map[string]string{"B": "{A}-b", "C": "{B}-c"}
	out, err := Derive(chain, map[string]string{"A": "a"}, nil)
	if err != nil || out["C"] != "a-b-c" {
		t.Fatalf("%v %v", out, err)
	}
	if _, err := Derive(map[string]string{"X": "{NOPE}"}, nil, nil); err == nil || !strings.Contains(err.Error(), "X can't be derived") {
		t.Fatalf("%v", err)
	}
}

func TestSecretFormats(t *testing.T) {
	for format, check := range map[string]func(string) bool{
		"hex:32":        func(v string) bool { return len(v) == 64 },
		"base64:32":     func(v string) bool { return len(v) == 44 && strings.HasSuffix(v, "=") },
		"base64url:32":  func(v string) bool { return len(v) == 43 && !strings.ContainsAny(v, "+/=") },
		"value:loom-rn": func(v string) bool { return v == "loom-rn" },
	} {
		f, err := ParseSecretFormat(format)
		if err != nil {
			t.Fatal(err)
		}
		v, err := f.Make()
		if err != nil || !check(v) {
			t.Errorf("%s made %q (%v)", format, v, err)
		}
	}
	for _, bad := range []string{"hex", "hex:4", "hex:9999", "rot13:5", "value:"} {
		if _, err := ParseSecretFormat(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDerivationNeverUsesSavedDerivedDependencies(t *testing.T) {
	got, err := Derive(map[string]string{"A": "{Z}", "Z": "{SOURCE}"}, map[string]string{
		"SOURCE": "new", "Z": "old", "A": "old",
	}, nil)
	if err != nil || got["A"] != "new" || got["Z"] != "new" {
		t.Fatalf("derived chain did not follow the new source: %v %v", got, err)
	}
	for _, cycle := range []map[string]string{{"A": "{A}"}, {"A": "{Z}", "Z": "{A}"}} {
		if _, err := Derive(cycle, map[string]string{"A": "old", "Z": "old"}, nil); err == nil {
			t.Fatal("saved values concealed a dependency cycle")
		}
	}
}

func TestCoreManifestMistakesAreNamed(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"aliases: [mail-broker]", "aliases: [processor]", "already processor's name"},
		{"        port: 4774", "        port: 99999", "must be a port number"},
		{"    order: 20", "    order: 20\n    routes: [{host: x.example.com}]", "no routes, port or schedule"},
		{"    grace: 60s", "    grace: 1h", "1s to 10m"},
		{"pids: 1024", "pids: 3", "16 or more"},
		{"      command: [sh, -ec, \"true\"]", "      command: [sh, -ec, \"true\"]\n      path: /health", "a path or a command"},
		{"    user: loom_cluster_admin", "    user: Loom-Admin", "lowercase letters, digits and underscores"},
		{"    init: deploy/postgres/init", "    init: ../etc", "inside the source"},
		{"MINIO_ROOT_USER: value:loom-root", "MINIO_ROOT_USER: rot13:5", "isn't hex, base64"},
		{"{LOOM_CORE_API_PASSWORD}@{postgres}", "{loom password}@{postgres}", "isn't a secret's name"},
		{"LOOM_VAULT_KEY: base64:32", "LOOM_OBJECTS_URL: base64:32", "both generated and derived"},
		{"  processor:\n    kind: worker", "  processor:\n    kind: worker\n    order: 3", "order is for release workloads"},
	}
	for _, c := range cases {
		text := strings.Replace(core, c.from, c.to, 1)
		if text == core {
			t.Fatalf("case %q didn't change the manifest", c.from)
		}
		if _, err := Parse([]byte(text)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want %q, got %v", c.to, c.want, err)
		}
	}
}
