BINARY := bridge
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build test vet lint cross clean

all: vet test build

build:
	go build -o $(BINARY) ./cmd/bridge

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

# Every release target must compile, including the ones not yet supported.
cross:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		printf '  %-16s' "$$t"; \
		GOOS=$$os GOARCH=$$arch go build -o /dev/null ./... && echo ok || exit 1; \
	done

clean:
	rm -f $(BINARY)
	rm -rf dist
