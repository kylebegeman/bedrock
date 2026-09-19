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

## Build

```sh
make check   # gofmt, vet, test
make build   # bin/quark for this Mac
make linux   # bin/quark-linux-amd64 for the machines
```

One static binary, no runtime to install.

## License

Apache 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
