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
#   make release-mac  - mac-app + dist MACAPP=1 + sign-mac + notarize-mac, checksums regenerated
#                       after signing
#   make release VERSION=x.y.z NOTES=<file> - the whole release: checks, local
#                       tag, dist, sign, notarize, verified checksums, store
#                       zip, then push the tag, the GitHub release and the
#                       Chrome Web Store upload. DRY_RUN=1 prints the steps.

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test vet lint spike extension extension-test store dist checksums sign-mac notarize-mac release-mac release mac-app macapp-zip-check

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
EXTENSION_FILES := manifest.json background.js ops.js send.js options.html options.js icon16.png icon48.png icon128.png

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

# MACAPP=1 builds the darwin binaries with the Agent Tincan.app zip that
# `make mac-app` wrote (build tag macapp); release sets it when signing.
dist: extension
	@if [ "$(MACAPP)" = 1 ]; then $(MAKE) -s macapp-zip-check; fi
	mkdir -p dist
	rm -f dist/tincan_* dist/checksums.txt
	for t in $(RELEASE_TARGETS); do \
		tags=; if [ "$(MACAPP)" = 1 ] && [ "$${t%/*}" = darwin ]; then tags="-tags macapp"; fi; \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build $$tags -ldflags "-s -w $(LDFLAGS)" -o dist/tincan_$${t%/*}_$${t#*/} ./cmd/tincan || exit 1; \
	done
	$(MAKE) checksums

macapp-zip-check:
	@[ -f "$(MACAPP_ZIP)" ] || { echo "make dist: MACAPP=1 needs $(MACAPP_ZIP); run make mac-app first" >&2; exit 1; }

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

# mac-app builds Agent Tincan.app, the bundle every tincan LaunchAgent starts
# through so macOS lists it in Login Items as "Agent Tincan" instead of the
# signer's name. It holds only cmd/agent-tincan-app (universal), Info.plist
# and the icon; it is signed, notarized and stapled, then zipped to
# internal/macapp/embedded/AgentTincan.zip, which `-tags macapp` builds of
# tincan embed. MACAPP_NOTARIZE=0 skips notarization for local testing.
MACAPP_DIR := dist/macapp
MACAPP_ICON ?= site/icon-192.png
MACAPP_NOTARIZE ?= 1
MACAPP_ZIP := internal/macapp/embedded/AgentTincan.zip

mac-app:
	@set -e; \
	app="$(MACAPP_DIR)/Agent Tincan.app"; \
	rm -rf "$(MACAPP_DIR)"; mkdir -p "$$app/Contents/MacOS" "$$app/Contents/Resources"; \
	for a in arm64 amd64; do \
		CGO_ENABLED=0 GOOS=darwin GOARCH=$$a go build -trimpath -ldflags "-s -w" -o "$(MACAPP_DIR)/agent-tincan_$$a" ./cmd/agent-tincan-app; \
	done; \
	lipo -create -output "$$app/Contents/MacOS/agent-tincan" "$(MACAPP_DIR)/agent-tincan_arm64" "$(MACAPP_DIR)/agent-tincan_amd64"; \
	sed 's/__VERSION__/$(VERSION)/' internal/macapp/Info.plist.tmpl > "$$app/Contents/Info.plist"; \
	plutil -lint "$$app/Contents/Info.plist" >/dev/null; \
	iconset="$(MACAPP_DIR)/AppIcon.iconset"; mkdir -p "$$iconset"; \
	for s in 16 32 128 256 512; do \
		sips -z $$s $$s "$(MACAPP_ICON)" --out "$$iconset/icon_$${s}x$${s}.png" >/dev/null; \
		d=$$((s * 2)); sips -z $$d $$d "$(MACAPP_ICON)" --out "$$iconset/icon_$${s}x$${s}@2x.png" >/dev/null; \
	done; \
	iconutil -c icns -o "$$app/Contents/Resources/AppIcon.icns" "$$iconset"; \
	security find-identity -v -p codesigning | grep -qF "$(TINCAN_SIGN_IDENTITY)" || { echo "make mac-app: codesign identity not found: $(TINCAN_SIGN_IDENTITY) (set TINCAN_SIGN_IDENTITY)" >&2; exit 1; }; \
	codesign --force --options runtime --timestamp --sign "$(TINCAN_SIGN_IDENTITY)" "$$app/Contents/MacOS/agent-tincan"; \
	codesign --force --options runtime --timestamp --sign "$(TINCAN_SIGN_IDENTITY)" "$$app"; \
	codesign --verify --strict --verbose=2 "$$app"; \
	if [ "$(MACAPP_NOTARIZE)" != 0 ]; then \
		zip="$(MACAPP_DIR)/notarize.zip"; ditto -c -k --keepParent "$$app" "$$zip"; \
		echo "notarizing $$app (waiting for Apple)"; \
		xcrun notarytool submit "$$zip" --keychain-profile "$(TINCAN_NOTARY_PROFILE)" --wait --output-format json > "$(MACAPP_DIR)/result.json" || { cat "$(MACAPP_DIR)/result.json" >&2; exit 1; }; \
		grep -q '"status": *"Accepted"' "$(MACAPP_DIR)/result.json" || { cat "$(MACAPP_DIR)/result.json" >&2; echo "make mac-app: the app was not accepted" >&2; exit 1; }; \
		xcrun stapler staple "$$app"; \
		xcrun stapler validate "$$app"; \
	fi; \
	mkdir -p "$$(dirname $(MACAPP_ZIP))"; rm -f "$(MACAPP_ZIP)"; \
	ditto -c -k --norsrc --noextattr --keepParent "$$app" "$(MACAPP_ZIP)"; \
	if unzip -Z1 "$(MACAPP_ZIP)" | grep -qE '(^|/)(\._|__MACOSX)'; then echo "make mac-app: $(MACAPP_ZIP) holds AppleDouble entries, which would break the app's seal when unpacked" >&2; exit 1; fi; \
	echo "make mac-app: wrote $(MACAPP_ZIP)"

release-mac:
	$(MAKE) mac-app
	$(MAKE) dist MACAPP=1
	$(MAKE) sign-mac notarize-mac
	$(MAKE) checksums
	cd dist && shasum -a 256 -c checksums.txt

# make release VERSION=x.y.z NOTES=<file> runs the whole release from a clean
# checkout of the remote's main. Nothing public happens until every asset is
# built, signed, notarized and verified: the tag is created locally first
# (and deleted again if a later step fails before the push), then pushed,
# then the GitHub release and the Chrome Web Store upload follow. The store
# upload is skipped, not failed, when extension/manifest.json is not newer
# than the store's version. DRY_RUN=1 runs the read-only checks, reports any
# that would stop the release, prints every step without running it, and
# exits non-zero when a check failed.
#
#   RELEASE_REMOTE  where the tag goes and main is compared (default origin;
#                   a URL works)
#   RELEASE_BRANCH  HEAD must equal this branch on RELEASE_REMOTE (default
#                   main; empty skips the check)
#   RELEASE_REPO    the GitHub repo for gh release create
#   SIGN=0          skip sign-mac and notarize-mac (no Mac certificate)
#   CWS=0           skip the Chrome Web Store upload
#   RELEASE_TINCAN  how to run the release tools (default go run ./cmd/tincan)
RELEASE_REMOTE ?= origin
RELEASE_BRANCH ?= main
RELEASE_REPO ?= mvanhorn/agent-tincan
RELEASE_TINCAN ?= go run ./cmd/tincan
RELEASE_ASSETS := dist/checksums.txt dist/tincan-history-extension.zip dist/tincan_darwin_amd64 dist/tincan_darwin_arm64 dist/tincan_linux_amd64 dist/tincan_linux_arm64
SIGN ?= 1
CWS ?= 1
DRY_RUN ?=

release:
	@set -e; \
	dry='$(filter-out 0,$(DRY_RUN))'; \
	if [ "$(origin VERSION)" != "command line" ]; then echo "make release: VERSION=x.y.z is required" >&2; exit 1; fi; \
	v='$(VERSION)'; \
	echo "$$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo "make release: VERSION must be x.y.z or x.y.z-pre, without a leading v (got $$v)" >&2; exit 1; }; \
	tag="v$$v"; \
	blocked=; \
	fail() { if [ -n "$$dry" ]; then blocked=1; echo "make release: dry run, the release would stop here: $$*" >&2; else echo "make release: $$*" >&2; exit 1; fi; }; \
	run() { echo "+ $$*"; if [ -z "$$dry" ]; then "$$@"; fi; }; \
	[ -n "$(NOTES)" ] || fail "NOTES=<file> is required (the release notes)"; \
	[ -z "$(NOTES)" ] || [ -f "$(NOTES)" ] || fail "notes file $(NOTES) does not exist"; \
	[ -z "$$(git status --porcelain)" ] || fail "the working tree is not clean"; \
	if [ -n "$(RELEASE_BRANCH)" ]; then \
		remote_head=$$(git ls-remote "$(RELEASE_REMOTE)" "refs/heads/$(RELEASE_BRANCH)" | cut -f1); \
		[ "$$(git rev-parse HEAD)" = "$$remote_head" ] || fail "HEAD is not $(RELEASE_BRANCH) on $(RELEASE_REMOTE) ($${remote_head:-unreadable}); check it out, or set RELEASE_BRANCH= to release this commit"; \
	fi; \
	if git rev-parse -q --verify "refs/tags/$$tag" >/dev/null; then fail "tag $$tag already exists locally"; fi; \
	remote_tag=$$(git ls-remote --tags "$(RELEASE_REMOTE)" "refs/tags/$$tag") || fail "cannot read the tags on $(RELEASE_REMOTE)"; \
	[ -z "$$remote_tag" ] || fail "tag $$tag already exists on $(RELEASE_REMOTE)"; \
	if [ -z "$$dry" ]; then command -v gh >/dev/null || fail "gh is not on PATH"; fi; \
	pre=; case "$$v" in *-*) pre=--prerelease;; esac; \
	pushed=; \
	cleanup() { if [ -z "$$dry" ] && [ -z "$$pushed" ] && git rev-parse -q --verify "refs/tags/$$tag" >/dev/null; then echo "make release: stopped before the tag was pushed; deleting the local tag $$tag" >&2; git tag -d "$$tag" >/dev/null; fi; }; \
	trap cleanup EXIT; \
	run git tag -a "$$tag" -m "$$tag"; \
	if [ "$(SIGN)" != 0 ]; then run $(MAKE) mac-app VERSION=$$v; run $(MAKE) dist VERSION=$$v MACAPP=1; else run $(MAKE) dist VERSION=$$v; fi; \
	if [ "$(SIGN)" != 0 ]; then run $(MAKE) sign-mac notarize-mac VERSION=$$v; fi; \
	run $(MAKE) checksums; \
	run sh -c 'cd dist && shasum -a 256 -c checksums.txt'; \
	run $(MAKE) store; \
	if [ "$(CWS)" != 0 ]; then run $(RELEASE_TINCAN) release-tools cws-upload --dry-run $(STORE_ZIP); fi; \
	run git push "$(RELEASE_REMOTE)" "refs/tags/$$tag"; \
	pushed=1; \
	run gh release create "$$tag" --repo "$(RELEASE_REPO)" --verify-tag --title "$$tag" --notes-file "$(NOTES)" $$pre $(RELEASE_ASSETS); \
	if [ "$(CWS)" != 0 ]; then run $(RELEASE_TINCAN) release-tools cws-upload --publish $(STORE_ZIP); fi; \
	if [ -z "$$dry" ]; then echo "make release: released $$tag"; \
	elif [ -n "$$blocked" ]; then echo "make release: dry run; nothing was run, and a check above would stop the release" >&2; exit 1; \
	else echo "make release: dry run; nothing was run"; fi
