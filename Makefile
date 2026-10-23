.PHONY: run build test vet

run:
	go run ./cmd/ptt-server

build:
	go build -o bin/ptt-server ./cmd/ptt-server

test:
	go test -race ./...

vet:
	go vet ./...
