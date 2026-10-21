BINARY := bin/vodlocal

.PHONY: all build build-linux run test vet fmt clean

all: fmt vet test build

build:
	go build -o $(BINARY) ./cmd/server

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BINARY)-linux-amd64 ./cmd/server

run:
	bash scripts/run-local.sh

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
