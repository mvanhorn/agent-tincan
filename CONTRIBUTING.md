# Contributing

Thanks for helping with Agent Tincan. Many PRs here are written by coding agents; this file is the short path for humans and agents alike. Read [docs/trust-model.md](docs/trust-model.md) before touching the relay, identity, wake, history or web-agent code: it lists the guarantees every change has to keep.

## Setup

Go 1.26+ (see `go.mod`). Node is needed only if you change the Chrome extension. From the repo root:

```bash
make build   # ./tincan
make test    # go test -race ./...
make vet
make lint    # golangci-lint run
make extension-test   # node --test for extension/, only when extension/ changed
```

You do not need Tailscale to run the tests. `internal/testrelay` runs a real relay behind local test servers that pin each caller's tailnet address, so relay, client and MCP tests run anywhere.

## Pull requests

1. Make the change and add or update tests. A bug fix should come with a test that fails without the fix.
2. PR titles use Conventional Commits (`fix(wake): ...`, `feat(cli): ...`, `docs: ...`); the `pr-title` check enforces it, and the title becomes the squash commit subject.
3. In the description, say what the problem was, what you changed, and how you tested it. If you ran it against a live relay or a real agent (Hermes, Codex, ChatGPT in Chrome), say so.
4. Do not bump versions, edit release notes or add release artifacts. Versions come from the git tag at release time.
5. Keep the protocol backward compatible. Agents on a mesh upgrade at different times, so a new relay must keep serving older clients and a new client must work against an older relay. New JSON fields and headers are optional; existing `/v1` routes and fields are not removed or renamed.
6. If you change behavior a user or agent sees, update the matching doc in the same PR: `README.md`, `docs/`, `docs/adapters/<platform>.md`, and `site/agents.txt` for setup steps agents follow.

First-time contributors: GitHub Actions waits for a maintainer to approve the first CI run, so checks may sit pending for a bit. The Vercel check fails on every fork PR (it needs team authorization) and can be ignored.

Every PR also gets an automated review from Greptile. Its repo-specific rules live in [`greptile.json`](greptile.json). Address its findings or reply explaining why one does not apply.

## Tests

```bash
go test -race ./...
go test ./internal/wake/ -run TestOnlineSkip -v
go test -cover ./internal/relay/
```

## Security

- Never commit real auth keys, relay keys, invite codes, wake.json secrets, OAuth tokens, cookies or chat content. Use dummy values in tests and fixtures.
- Report vulnerabilities privately to [@mvanhorn](https://github.com/mvanhorn), not in a public issue.

## Releases (maintainers)

Tag `vX.Y.Z` on main, run `make release-mac` (builds, signs and notarizes), then `gh release create` with the files in `dist/`. Relays that serve `--dist` hand the new build to `tincan upgrade`.
