# agent-tincan Makefile
#
#   make build   - static build of ./cmd/tincan -> ./tincan
#   make test    - go test -race ./...
#   make vet     - go vet ./...
#   make lint    - golangci-lint run
#   make spike   - cross-compile the U1 spike binaries into spike/bin/
#   make extension      - package extension/ into dist/tincan-history-extension.zip
#   make extension-test - node --test for the extension's worker code
#   make store   - Chrome Web Store upload zip (no manifest "key") in
#                  dist/tincan-history-extension-store.zip, with its sha256
#   make dist    - every release asset in dist/: tincan_<os>_<arch> for the
#                  four release targets, checksums.txt, and the extension zip
#   make sign-mac     - codesign dist/tincan_darwin_* with the Developer ID
#                       identity (hardened runtime, timestamp), then rewrite
#                       checksums.txt
#   make notarize-mac - submit each signed dist/tincan_darwin_* to Apple's
#                       notary service and wait for the verdict
#   make release-mac  - dist + sign-mac + notarize-mac, checksums regenerated
#                       after signing

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test vet lint spike extension extension-test store dist checksums sign-mac notarize-mac release-mac

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
EXTENSION_FILES := manifest.json background.js ops.js send.js icon16.png icon48.png icon128.png

extension:
	mkdir -p dist
	rm -f dist/tincan-history-extension.zip
	cd extension && zip -X -q ../dist/tincan-history-extension.zip $(EXTENSION_FILES)

# The Web Store rejects a manifest with a "key" (the store assigns the id),
# so the store zip carries a copy of the manifest without it;
# extension/manifest.json and the release zip keep the key, so the
# unpacked install keeps its fixed id.
STORE_ZIP ?= dist/tincan-history-extension-store.zip

store:
	@set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for f in $(EXTENSION_FILES); do cp "extension/$$f" "$$tmp/$$f"; done; \
	node -e 'const fs=require("fs");const f=process.argv[1];const m=JSON.parse(fs.readFileSync(f,"utf8"));delete m.key;fs.writeFileSync(f,JSON.stringify(m,null,2)+"\n")' "$$tmp/manifest.json"; \
	mkdir -p "$$(dirname "$(STORE_ZIP)")"; out=$$(cd "$$(dirname "$(STORE_ZIP)")" && pwd)/$$(basename "$(STORE_ZIP)"); \
	rm -f "$$out"; (cd "$$tmp" && zip -X -q "$$out" $(EXTENSION_FILES)); \
	shasum -a 256 "$(STORE_ZIP)"

extension-test:
	node --test 'extension/test/*.test.js'

# Release targets: the raw binaries the release page and the relay's --dist
# directory carry, stripped (-s -w) like the published releases.
# checksums.txt covers the binaries.
RELEASE_TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

dist: extension
	mkdir -p dist
	rm -f dist/tincan_* dist/checksums.txt
	for t in $(RELEASE_TARGETS); do \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags "-s -w $(LDFLAGS)" -o dist/tincan_$${t%/*}_$${t#*/} ./cmd/tincan || exit 1; \
	done
	$(MAKE) checksums

checksums:
	cd dist && shasum -a 256 tincan_* > checksums.txt

# macOS signing and notarization (optional; make dist works without a
# certificate). Signing changes the binaries' bytes, so sign-mac rewrites
# checksums.txt. Bare Mach-O binaries cannot be stapled: notarize-mac zips
# each one for submission, and Gatekeeper finds the ticket online at first
# launch. One-time notarytool setup:
#   xcrun notarytool store-credentials <profile> --apple-id <id> --team-id <team>
TINCAN_SIGN_IDENTITY ?= Developer ID Application: Matthew Charles Van Horn (NM8VT393AR)
TINCAN_NOTARY_PROFILE ?= agentcookie-notary

sign-mac:
	@set -e; ls dist/tincan_darwin_* >/dev/null 2>&1 || { echo "make sign-mac: no dist/tincan_darwin_* binaries; run make dist first" >&2; exit 1; }; \
	security find-identity -v -p codesigning | grep -qF "$(TINCAN_SIGN_IDENTITY)" || { echo "make sign-mac: codesign identity not found: $(TINCAN_SIGN_IDENTITY) (set TINCAN_SIGN_IDENTITY)" >&2; exit 1; }; \
	for f in dist/tincan_darwin_*; do \
		echo "signing $$f"; \
		codesign --force --options runtime --timestamp --sign "$(TINCAN_SIGN_IDENTITY)" "$$f"; \
		codesign --verify --strict --verbose=2 "$$f"; \
	done
	$(MAKE) checksums

notarize-mac:
	@set -e; ls dist/tincan_darwin_* >/dev/null 2>&1 || { echo "make notarize-mac: no dist/tincan_darwin_* binaries; run make dist sign-mac first" >&2; exit 1; }; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	for f in dist/tincan_darwin_*; do \
		codesign -dv "$$f" 2>&1 | grep -q 'flags=.*runtime' || { echo "make notarize-mac: $$f is not signed with the hardened runtime; run make sign-mac" >&2; exit 1; }; \
		zip="$$tmp/$$(basename "$$f").zip"; \
		ditto -c -k --keepParent "$$f" "$$zip"; \
		echo "notarizing $$f (waiting for Apple)"; \
		xcrun notarytool submit "$$zip" --keychain-profile "$(TINCAN_NOTARY_PROFILE)" --wait --output-format json > "$$tmp/result.json" || { cat "$$tmp/result.json" >&2; exit 1; }; \
		cat "$$tmp/result.json"; echo; \
		grep -q '"status": *"Accepted"' "$$tmp/result.json" || { echo "make notarize-mac: $$f was not accepted; see xcrun notarytool log <id> --keychain-profile $(TINCAN_NOTARY_PROFILE)" >&2; exit 1; }; \
		codesign -dvv "$$f" 2>&1 | grep -E 'Authority=Developer ID Application|TeamIdentifier'; \
		spctl -a -vv -t install "$$f" 2>&1 | grep -q 'source=Notarized Developer ID' && echo "$$f: Gatekeeper source=Notarized Developer ID" || \
			echo "make notarize-mac: warning: spctl does not report $$f as notarized yet (the ticket can take a few minutes to propagate)" >&2; \
	done

release-mac: dist sign-mac notarize-mac
	$(MAKE) checksums
	cd dist && shasum -a 256 -c checksums.txt
