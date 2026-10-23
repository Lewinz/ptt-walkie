.PHONY: run demo build test vet

run:
	go run ./cmd/ptt-server

demo:
	PTT_DEMO=1 go run ./cmd/ptt-server

build:
	go build -o bin/ptt-server ./cmd/ptt-server
	go build -o bin/ptt-sim ./cmd/ptt-sim

test:
	go test -race ./...

vet:
	go vet ./...
