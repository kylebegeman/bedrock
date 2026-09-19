.PHONY: build test check linux lane-reset lane-status

build:
	CGO_ENABLED=0 go build -trimpath -o bin/quark ./cmd/quark

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/quark-linux-amd64 ./cmd/quark

test:
	go test ./...

check:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

# The test bed: wipe the Hostinger machine and wait for SSH. See lane/README.md.
lane-reset:
	lane/reset.sh

lane-status:
	lane/reset.sh --status
