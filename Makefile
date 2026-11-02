.PHONY: build test lint

build:
	go build -o bin/szwm ./cmd/szwm

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
