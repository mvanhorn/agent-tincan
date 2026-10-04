---
title: Accurate Stock Good-At Lines - Plan
type: fix
date: 2026-10-03
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Accurate Stock Good-At Lines - Plan

---

## Goal Capsule

- Objective: on every Tincan install, an agent choosing a teammate reads a default good-at line that matches what that teammate can actually do, so work stops going to teammates that cannot do it (such as an image job sent to claude-web).
- Means: rewrite the stock lines from verified adapter and product behavior, add lines for product-tool kinds, and resolve a missing stored kind from the agent's product name (KTD1, KTD2).
- Authority: this plan's Requirements, then its KTDs. Where it differs from `docs/plans/2026-10-03-0800-feat-teammate-good-at-lines-plan.md` (R5, KTD2 and the name-keyed "not built" item), this plan wins.
- Stop conditions: stop and ask if a stock line cannot fit 120 runes without dropping a capability an asker relies on, or if resolving a kind from the name would change anything besides the good-at line.
- Execution profile: one PR. It replaces PR #118, which is closed with a link to the new PR.

---

## Product Contract

### Summary

Correct every stock good-at line against the code and the products, add stock lines for the product tools (Claude Code, Codex, Gemini CLI, Grok CLI and the ChatGPT connector), and show those lines even when an agent joined without a stored kind. Fix the instruction text and docs that repeat the same wrong claims.

### Problem Frame

Stock lines ship to every install and are the main input agents use to pick a teammate when the owner has not written a line. Several are wrong. claude-web claims generated images Claude never makes. history omits Claude Code and Grok CLI, two of the eight sources it reads, and its "Claude" could mean claude.ai or Claude Code. council claims "every model on the team" though Claude Code, services and unwakeable agents never sit. gemini-web states image attachment that is unconfirmed on the live site. The same image claims sit in the agent instructions template (claude-web and copilot-web), the web-agents doc and the site. Product-tool agents show no line at all, and agents joined without `--kind`, which the claude-code adapter doc tells people to do, would show none even if one existed.

### Requirements

Line content

- R1. Each stock line states only capabilities the Tincan adapter delivers and the product reliably has today, in plain prose of at most 120 runes with no em dash, en dash or bold.
- R2. A line names a limit only when an asker would otherwise wrongly assume the capability: claude-web makes no images, grok-web attaches no videos, dot-web and council asks are held for the owner's approval by default, claude-code answers only while a session is open, and the ChatGPT connector cannot pick up asks later.
- R3. The history line names all eight sources it reads (ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code, Grok CLI), keeps the word "ChatGPT", and keeps its images claim (seven of the eight sources return images; Copilot does not).

Which kinds get a line

- R4. Service kinds (history, notes, council and the seven web kinds) keep a stock line, and a new service kind cannot ship without one.
- R5. The product-tool kinds claude-code, codex, gemini-cli, grok-cli and chatgpt get a stock line describing the tool as Tincan runs it.
- R6. Hosting-shape kinds (vm-webhook, e2b-email, proxy-sandbox, scheduled, generic) and the framework kinds hermes and openclaw get no stock line.
- R7. An agent with no stored kind whose name is a known product runtime name (for example `codex` or `claude-code`) shows that product kind's stock line.

Consistency

- R8. The agent instructions, adapter docs, README, protocol doc and site no longer claim generated images for claude-web or copilot-web, and they describe which agents get stock lines in agreement with R4 to R6.

### Key Decisions

- Product kinds get lines; hosting shapes do not. The product behind a hosting shape differs per owner, so a default would mislead. Owner-specific facts such as "Muse calls businesses only" and "Fo has a human make calls" stay owner lines. Governs R5, R6. (session-settled: user-approved — chosen over lines for every kind: a hosting-shape default would describe someone else's agent)
- Hermes and OpenClaw get no stock line. Their model, tools and skills are owner-configured, so a line could only repeat the `kind=` the roster already shows. Governs R6. (session-settled: user-directed — chosen over a generic framework line: it tells other agents nothing they can act on)
- Lines state capabilities and name a limit only when it is one an asker would assume away. Governs R1, R2. (session-settled: user-approved — chosen over listing gaps: gaps crowd out what the agent can do within 120 runes)

### Scope Boundaries

- Not changed: how owner lines are set, stored or validated, or the 120-rune cap.
- Not changed: the owner's own lines on any relay.
- Considered and not built: a roster flag marking a line as stock. Agents still choose by the text; revisit if owners report they cannot tell their lines from defaults now that more agents have one.
- Considered and not built: per-site claims that depend on the owner's paid tier (Gemini video, Perplexity Pro images). Tier varies per owner and the adapter does not attach video, so these stay out of the lines.

### Sources

- Adapter evidence: `internal/history/claudeai.go` (claude.ai images are uploads only), `internal/history/copilot.go` (no file operation), `internal/history/sites.go`, `docs/adapters/web-agents.md` (Grok videos not attached; Gemini image fetch unconfirmed; Copilot "Not in v1"; dot approval hold), `docs/adapters/council.md` (membership and approval), `internal/history/history.go` (eight sources), `docs/adapters/codex.md`, `docs/adapters/grok-cli.md`, `docs/adapters/gemini-cli.md` (Antigravity CLI is the default engine), `docs/adapters/chatgpt.md`.
- Product evidence (web, checked 2026-10-03): Claude has no native image generation; ChatGPT (Images 2.0), Grok (Imagine) and Gemini (Nano Banana 2, Veo 3.1 on paid plans) generate images; OpenAI dots act in connected apps; Hermes Agent and OpenClaw are self-hosted, owner-configured agents.

---

## Planning Contract

### Key Technical Decisions

- KTD1. Resolve a stock line's kind as the stored kind, else the product runtime name, using the existing `runtimeNames` map in `internal/onboard/onboard.go`, never the generic fallback. That map already holds only product names, never personal agent names, so a name match is safe. The lookup stays a pure function of name and kind, and the relay's `handleAgents` calls it instead of `StockGoodAt(a.Kind)`. Governs R7. Supersedes the "stock lines keyed on an agent's name" non-goal in the earlier good-at plan.
- KTD2. The stock map's key set is pinned by an explicit test list, not derived from `isService`. `isService` also drives config paths, council exclusion and instruction blocks, so it stays unchanged. The guard asserts that every `isService` kind has a line (R4), that the product kinds in R5 have lines, and that the R6 kinds have none.
- KTD3. The lines are fixed in this plan (table below) and validated by the existing hygiene test, which runs every line through `SetGoodAt`. An implementer may tighten wording to fit 120 runes but must keep each line's claims and named limits.

| Kind | Stock line |
|---|---|
| history | finds the owner's past ChatGPT, claude.ai, Grok, Gemini, Copilot, Codex, Claude Code and Grok CLI chats and images |
| notes | saves, searches and reads the owner's Agent Notes on their Mac; never edits or deletes one (unchanged) |
| council | asks web, CLI and webhook agents (not Claude Code) one question, ranks answers blind; slow, held for approval by default |
| chatgpt-web | asks ChatGPT (chatgpt.com) in the owner's browser and replies with the answer and any images ChatGPT made |
| claude-web | asks Claude (claude.ai) in the owner's browser and replies with the answer as text; Claude makes no images |
| grok-web | asks Grok (grok.com) in the owner's browser and replies with the answer and any images Grok made; no videos |
| gemini-web | asks Gemini (gemini.google.com) in the owner's browser; it can draw on their connected Gmail, Drive and Calendar |
| perplexity-web | unchanged |
| copilot-web | unchanged |
| dot-web | asks the owner's OpenAI dot (chatgpt.com/dots), which acts in their connected apps; held for approval by default |
| claude-code | Claude Code in a terminal on the owner's machine: reads, edits and runs code; answers only while a session is open |
| codex | OpenAI Codex run unattended on the owner's machine: reads code anywhere, edits and runs it in its own work folder |
| gemini-cli | a Gemini coding agent run unattended on the owner's machine: reads, edits and runs code |
| grok-cli | xAI's Grok Build CLI run unattended on the owner's machine: reads code, edits and runs it in its own work folder |
| chatgpt | ChatGPT in the owner's ChatGPT app; acts only while the owner is chatting with it, so it cannot pick up asks later |

### Assumptions

- gemini-web images: the line leaves out images because live image attachment is unconfirmed (`docs/adapters/web-agents.md`). Once a live run confirms it, adding "and any images Gemini made" is a one-line follow-up.
- gemini-cli is described as "a Gemini coding agent" because the default engine is the Antigravity CLI and the alternative is Gemini CLI. Its line names no write scope: the default engine runs unconfined, unlike codex and grok-cli, whose writes stay in their work folder.

---

## Implementation Units

### U1. Stock lines and kind resolution

- **Goal:** ship the corrected and new stock lines and show them for agents without a stored kind.
- **Requirements:** R1 to R7; KTD1, KTD2, KTD3.
- **Dependencies:** none.
- **Files:** `internal/onboard/onboard.go`, `internal/onboard/goodat_test.go`, `internal/relay/server.go`, `internal/relay/goodat_test.go` (or the existing relay good-at test file), `internal/client/relay_test.go`.
- **Approach:**
  1. Replace the `stockGoodAt` entries with the KTD3 table and add the five product kinds.
  2. Add the name-aware lookup from KTD1 next to `StockGoodAt`, and update the map comment that says general kinds have no stock line.
  3. Switch `handleAgents` to the name-aware lookup, keeping owner lines first.
  4. Rewrite `TestStockGoodAtCoversExactlyTheServiceKinds` per KTD2. Its current assertion that codex has no line is replaced by the explicit lists.
- **Patterns to follow:** per-kind data as map entries beside `runtimeNames` and `defaultWake`; `resolveKind` for the stored-then-name order, minus its generic fallback.
- **Test scenarios:**
  - Every `isService` kind has a non-empty stock line.
  - claude-code, codex, gemini-cli, grok-cli and chatgpt each have a non-empty stock line.
  - vm-webhook, e2b-email, proxy-sandbox, scheduled, generic, hermes and openclaw return an empty line.
  - Every stock line passes `SetGoodAt` validation and contains no em dash, en dash or `**` (existing hygiene test, now covering the new keys).
  - The history line contains "ChatGPT", "claude.ai", "Claude Code", "Grok CLI" and "images".
  - The claude-web line contains "no images" and does not contain "generated images".
  - Name-aware lookup: name `codex` with no stored kind returns the codex line.
  - Name-aware lookup: name `muse` with no stored kind returns empty, because muse is not a product name and there is no generic fallback.
  - Name-aware lookup: stored kind `vm-webhook` with name `codex` returns empty, because a stored kind wins over the name.
  - Relay roster: an agent named `claude-code` joined without a kind shows the claude-code stock line in `GET /v1/agents`.
  - Relay roster: an owner line on that agent replaces the stock line.
  - `TestGoodAtLinesThroughClient` still passes with the new history line.
- **Verification:** the onboard, relay and client good-at tests pass, and `TestGoodAtParityAcrossCLIAndMCP` still counts the same lined agents because test-relay agents have no product names.

### U2. Instructions, docs and site agree with the lines

- **Goal:** remove the wrong image claims everywhere and describe which agents get stock lines.
- **Requirements:** R8.
- **Dependencies:** U1, for the final line wording.
- **Files:** `internal/onboard/templates/agent.tmpl`, `internal/onboard/web_test.go`, `internal/mcpserver/server.go`, `docs/adapters/web-agents.md`, `docs/adapters/claude-code.md`, `README.md`, `docs/protocol.md`, `site/agents.txt`, `site/index.html`, `internal/cli/onboard.go`, `internal/cli/web.go`, `internal/client/relay.go`.
- **Approach:**
  1. In `instructions.web`, give claude-web "replies with the answer" with no image clause and say Claude makes no images, and give copilot-web text plus its Sources sentence with no image clause. ChatGPT and Grok keep the images clause, and Gemini's says images are attached when they can be fetched, matching the best-effort site wording.
  2. Fix the image claims in `docs/adapters/web-agents.md` (the claude-web "does the same" sentence), `site/index.html` (the Claude and Gemini image sentences, keeping Gemini's only if worded as best effort) and `README.md` (the web-agents paragraph PR #118 touched).
  3. Rewrite the stock-line descriptions in `README.md` (two places), `docs/protocol.md`, `site/agents.txt`, the `tincan good-at` help text and the `client/relay.go` and `relay/server.go` comments: services and product tools show a default line, and hosting shapes, Hermes and OpenClaw show none until the owner sets one.
  4. Change the `list_agents` description from "the good_at line the owner wrote" to say the line is the owner's or a default.
  5. Add `--kind claude-code` to the invite in `docs/adapters/claude-code.md`.
  6. In the `tincan web` help (`internal/cli/web.go`, the long description and the serve steps), say generated images come back from ChatGPT, Grok and Gemini only, and that Perplexity and Copilot answers end with their source links.
- **Patterns to follow:** existing per-kind `if eq .Kind` branches in `agent.tmpl`.
- **Test scenarios:**
  - The rendered claude-web instructions do not contain "generated images".
  - The rendered copilot-web instructions do not contain "generated images" and still contain "Sources:".
  - The rendered chatgpt-web and grok-web instructions still contain "generated images".
  - The rendered gemini-web instructions qualify the images clause as best effort.
  - The perplexity-web assertions in `web_test.go` still hold.
- **Verification:** a repo grep finds no remaining claim that claude-web or copilot-web returns generated images, including generic "every site" wording such as the `tincan web` help, and every doc that lists stock-line kinds matches R4 to R6.

---

## Verification Contract

| Gate | Command or check | Proves |
|---|---|---|
| Unit and integration tests | `go test ./...` | U1 and U2 scenarios, plus no regression elsewhere |
| Race | `go test -race ./internal/relay/ ./internal/onboard/` | roster lookup under the relay lock |
| Vet and lint | `go vet ./...`, `make lint` (6 known modernize issues, none new) | code hygiene |
| Static build | `GOOS=linux go build ./...` | Linux relay builds |
| Line audit | the hygiene test, plus reading each line in the KTD3 table against its cited source | R1, R2 |
| Live check | after release, `tincan agents` on the owner's relay shows the new defaults for agents without owner lines | R5, R7 end to end |

---

## Definition of Done

- Every line in the KTD3 table is shipped, within 120 runes, and passes the hygiene test.
- Agents of the R5 kinds show their line with or without a stored kind; R6 kinds show none.
- No instruction, doc or site text claims generated images for claude-web or copilot-web.
- PR #118 is closed with a link to the replacing PR, and its branch is deleted.
- No leftover experimental code or unused helpers in the diff.

### Documentation / Operational Notes

- Release notes for the next version: "default good-at lines corrected and added for Claude Code, Codex, Gemini CLI, Grok CLI and the ChatGPT connector". Owners who set their own lines see no change.
- The relay serves stock lines, so the fix reaches agents when the relay is upgraded; clients need no upgrade.
