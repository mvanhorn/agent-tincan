# agent-tincan Makefile
#
#   make build   - static build of ./cmd/tincan -> ./tincan
#   make test    - go test -race ./...
#   make vet     - go vet ./...
#   make lint    - golangci-lint run
#   make spike   - cross-compile the U1 spike binaries into spike/bin/
#   make extension      - package extension/ into dist/tincan-history-extension.zip
#   make extension-test - node --test for the extension's worker code
#   make dist    - every release asset in dist/: tincan_<os>_<arch> for the
#                  three release targets, checksums.txt, and the extension zip

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test vet lint spike extension extension-test dist

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o tincan ./cmd/tincan

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

spike:
	mkdir -p spike/bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o spike/bin/spike-relay-linux-amd64 ./spike/relay
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o spike/bin/spike-relay-linux-arm64 ./spike/relay
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o spike/bin/spike-poller-linux-amd64 ./spike/poller
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o spike/bin/spike-poller-linux-arm64 ./spike/poller
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o spike/bin/spike-poller-darwin-arm64 ./spike/poller

# The zip holds only what Chrome loads; tests and package.json stay out.
EXTENSION_FILES := manifest.json background.js ops.js send.js

extension:
	mkdir -p dist
	rm -f dist/tincan-history-extension.zip
	cd extension && zip -X -q ../dist/tincan-history-extension.zip $(EXTENSION_FILES)

extension-test:
	node --test 'extension/test/*.test.js'

# Release targets: the raw binaries the release page and the relay's --dist
# directory carry, stripped (-s -w) like the published releases.
# checksums.txt covers the binaries.
RELEASE_TARGETS := darwin/arm64 linux/amd64 linux/arm64

dist: extension
	mkdir -p dist
	rm -f dist/tincan_* dist/checksums.txt
	for t in $(RELEASE_TARGETS); do \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags "-s -w $(LDFLAGS)" -o dist/tincan_$${t%/*}_$${t#*/} ./cmd/tincan || exit 1; \
	done
	cd dist && shasum -a 256 tincan_* > checksums.txt
