# Quark

One small program that runs your machines. Quark installs and operates
everything a server needs for the apps it hosts: builds, certificates,
secrets, backups, health checks and alerts. Loom is where you see and approve
it all.

Quark is the Go rewrite of Ophelia. The Python 0.6 line is archived on the
`ophelia-0.6` branch and the `ophelia-0.6-final` tag, and stays there.

## Status

0.7 is being built. The plan, the ideas it was chosen from, and the blueprint
behind it live in [docs/](docs/):

- [docs/plan.html](docs/plan.html): the 0.7 plan, milestone by milestone.
- [docs/ideas.html](docs/ideas.html): the 37 ideas, with the 34 that were chosen.
- [docs/blueprint.html](docs/blueprint.html): why the rewrite, and what 0.7 to 1.0 are.

Every milestone is proven on a real machine that is wiped and rebuilt for the
purpose. See [lane/README.md](lane/README.md).

## An app's manifest

An app describes itself in `quark.yaml` at the root of its source:

```yaml
app: hello
description: What this is for
owner: personal
workloads:
  web:
    kind: web              # web, static or worker
    build: {context: .}    # or image: ghcr.io/you/hello:1.2
    port: 8000
    routes:
      - host: hello.example.com
      - host: example.com
        path: /hello       # a prefix; longest wins on a host
    env: {GREETING: hello}
    secrets: [API_KEY]     # names only; values live in quark's store
    health: {path: /healthz}
    resources: {memory: 256m, cpus: 0.5}
  site:
    kind: static
    dir: public            # served by a file server
    routes: [{host: www.example.com}]
checks:
  - url: https://hello.example.com/
    contains: hello
```

`quark deploy <dir>` builds the images on the machine, starts the new
revision beside the old one, runs the checks against it, confirms DNS,
switches the edge, waits for certificates, checks again through the edge
and retires the replaced revision, which `quark rollback` can bring back.
Every container is told its `QUARK_APP`, `QUARK_WORKLOAD` and
`QUARK_REVISION`.

## Build

```sh
make check   # gofmt, vet, test
make build   # bin/quark for this Mac
make linux   # bin/quark-linux-amd64 for the machines
```

One static binary, no runtime to install.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
