# Contributing To Bedrock

Bedrock is one Go program (Go 1.26, no cgo) that runs a machine. Changes
are small, proven where they can be, and written in plain prose: an error
says what to do next, a plan says what will change.

## Development

```sh
make check   # gofmt, go vet, go test ./...
make build   # bin/bedrock for this Mac
make linux   # bin/bedrock-linux-amd64 for the machines
```

CI runs `make check` and cross-builds for linux/amd64 and darwin/arm64 on
every push and pull request. The tests use the standard library only, real
SQLite stores in temporary directories, and fakes for the machine, Docker's
API and Cloudflare's; `go test ./...` touches no network and no real
Docker. A feature is proven on the lane, a real machine that is wiped and
rebuilt for the purpose; see [lane/README.md](lane/README.md).

## What a change looks like

- Fixture apps and `example.com` hostnames in tests and docs; never a real
  hostname, address, path, credential or production value.
- Anything that changes a machine is an operation: planned as steps,
  journaled, finished with a receipt. It takes `--plan` and `--digest`, and
  its plan must not depend on the moment it was made.
- Secret values never reach an argument, a log, a receipt or a message;
  only their names do.
- A `--json` output shape is a contract: change one deliberately and say so
  in the commit.
- Tests are sentences that name the behaviour, such as
  `TestAPlannedPreviewCopiesNoSecrets`.
- Commit subjects are sentences too, and the body says why.

## Pull requests

Run `make check`, say what changes for a person using bedrock and how it
was verified, and keep a pull request to one subject. Bedrock is licensed
under the Apache License 2.0; a contribution is licensed the same way.
