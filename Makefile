.PHONY: build test check linux lane-check lane-up lane-down lane-proof lane-reset lane-status

build:
	CGO_ENABLED=0 go build -trimpath -o bin/bedrock ./cmd/bedrock

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/bedrock-linux-amd64 ./cmd/bedrock

test:
	go test ./...

check: lane-check
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

# Disposable Linux proofs. Provisioning is separate from proof execution.
lane-check:
	lane/check.sh

lane-up:
	lane/vm.sh up

lane-down:
	lane/vm.sh down

lane-proof:
	lane/prove.sh m1

lane-reset:
	lane/reset.sh

lane-status:
	lane/reset.sh --status
