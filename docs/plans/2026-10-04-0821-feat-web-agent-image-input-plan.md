---
title: Web Agent Image Input - Plan
type: feat
date: 2026-10-04
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Web Agent Image Input - Plan

## Goal Capsule

- Objective: attached images reach supported composers before text submission; unsupported inputs receive an explicit reply.
- Means: authorized relay downloads, bounded native transfer, fixed composer operations and verified site capabilities.
- Authority: R-IDs and KTDs below; owner-reported incident and code at b983494. Unproven upload behavior is labeled and gated.
- Stop conditions: ChatGPT must pass live acceptance. Defer sites requiring arbitrary URLs/scripts, exported credentials or broader browser permissions.
- Execution profile: its own PR, independent of signed-out status. Planning performs no implementation, live sends or shipping.

## Product Contract

### Summary

Download attachments on the authorized web service, transfer bounded image bytes to the extension and let a fixed function attach them in an extension-owned composer tab. Send the text only after every image is ready. Keep unsupported sites explicit and preserve the current reply-image path.

### Problem Frame

The owner reports that chatgpt-web received an ask to edit a meme with the cropped image attached, but ChatGPT answered that it could not see an image. `envelope.Request.Attachments` already carries relay attachment metadata. `WebAgent.Handle` checks the chain and sends `wr.message` through `Client.Send`; that method constructs OpArgs with text and threading only. `extension/ops.js` SEND_SPEC and `extension/send.js` drive text entry and submission. Existing file operations fetch images out of conversations, not into their composers. The missing work is the complete input path, not relay attachment storage.

### Requirements

Input and failure behavior

- R1. Upload every accepted image before text submission on enabled sites, in new and continued conversations. ChatGPT is required; other sites need the same proof.
- R2. Fail the entire unsent ask if any file is unsupported, missing, corrupt, oversized or rejected. Name the file and reason; never silently drop it or send partial context.
- R3. Proposed limits: four PNG/JPEG images, 10 MiB each, 20 MiB total decoded bytes; honor lower relay/site limits. No resizing, conversion, SVG, animation, PDF, arbitrary files or empty prompts.

Trust and lifecycle

- R4. Fetch relay IDs as the recipient only after claim and chain authorization. Held requests stay inaccessible until approved; attachments receive no approval bypass.
- R5. Fixed operations accept bounded data only, never URLs, local paths, selectors, code or exported browser tokens. Preserve host grants and owned-tab restrictions.
- R6. Serialize requests and preserve journal no-resend behavior. Distinguish before-click failure, files already uploaded and after-click uncertainty; never automatically retry an uncertain send.
- R7. Old hosts/extensions fail image asks with an upgrade instruction before typing. Disabled sites fail before fetch or composer mutation. Text-only operations remain compatible.

Coverage and documentation

- R8. Evaluate all seven sites; publish enabled/deferred evidence. Unproven sites stay disabled. Input support is independent of generated-image output.
- R9. Update adapter docs, README and internal/onboard/templates/agent.tmpl with enabled kinds, limits, exposure and failures. Show CLI --attach and local MCP attach; retain remote MCP local-path limits.

### Key Decisions

- All files must be ready before text submission. Governs R1, R2.
- Use fixed composer functions, not new vendor upload APIs; File/DataTransfer feasibility is unproved. Governs R1, R5, R8.
- Chunk bytes across the existing 256 KiB service and 1 MiB host-frame limits; existing file operations download in the opposite direction. Governs R3, R5.
- ChatGPT is mandatory. Other kinds require evidence, not inference from image output. Governs R1, R8.

### Scope Boundaries

- Direct CLI/MCP asks with relay attachments, including approved dot inbound asks if proved. Existing chain checks, holds and threading remain authoritative.
- Considered and not built: arbitrary files, conversion, URL substitutes, direct vendor upload APIs, debugger permission, OS file pickers or generic extension plugins.
- Considered and not built: dot outbound attachment forwarding. internal/history/dots.go retains counts, not downloadable IDs.
- Considered and not built: generated-image output changes, health reporting or council attachment fan-out changes.

### Sources

- Service/transport: `internal/envelope/envelope.go`, `internal/history/web.go`, `internal/history/web_journal.go`, `internal/history/native.go`, `internal/history/sites.go`, `internal/cli/web.go`, `extension/background.js`, `extension/ops.js`, `extension/send.js` and their tests.
- Fetch/approval: `internal/client/attachments.go`, `internal/relay/attachments.go`, `internal/store/attachments.go`, `internal/relay/server.go`, `internal/policy/approval.go`.
- Site evidence: `internal/history/chatgpt.go`, `internal/history/claudeai.go`, `internal/history/grok.go`, `internal/history/gemini.go`, `internal/history/perplexity.go`, `internal/history/copilot.go`, `internal/history/dots.go`.
- Surfaces: `internal/mcpserver/server.go`, `internal/client/render.go`, `internal/cli/agent.go`, `docs/adapters/web-agents.md`, `docs/protocol.md`, `README.md`, `internal/onboard/templates/agent.tmpl`; commands: `Makefile`, `extension/package.json`.

## Planning Contract

### Key Technical Decisions

- KTD1. Add image-input capability separate from output flags. Loaded-code hello advertises enabled sites and transport version through host.status. Missing capability fails closed; Go and extension both reject disabled sites. Governs R1, R7, R8.
- KTD2. Use Relay.DownloadAttachment and a writer enforcing remaining request budget. Check relay SHA256, actual size, PNG/JPEG magic, MIME consistency and bounded image DecodeConfig, with a proposed 40-million-pixel cap. Stage generated-ID files in a 0700 request directory with 0600 files; clean every exit. Governs R2 to R4.
- KTD3. Add fixed per-site input begin/chunk/abort and image-send operations. Begin validates names/MIME/sizes/hashes and returns an opaque token. Acknowledged sequential chunks carry at most 96 KiB decoded, fitting base64/JSON under the 256 KiB service cap and 1 MiB Chrome cap. Worker limits: four pending transfers, 40 MiB pending decoded bytes, two-minute idle and ten-minute absolute expiry. Check token/site/sequence/size/hash before consumption. Governs R3, R5, R7.
- KTD4. Bind tokens to initiating Unix connection and site using a request-scoped persistent connection. Replace the current one-frame serve/disconnect reader for this path so it cannot consume subsequent frames. Disconnect aborts unconsumed transfers; reconnect loses state and fails closed. Tokens are single-use and bytes never persist in the worker. Pending transfers delay reload only to the existing deadline. Log no bytes/hashes/private names. Governs R5 to R7.
- KTD5. Extend the queued send: authenticate, open owned tab, check thread/draft, attach, await every ready/error signal, fill/verify text, recheck readiness, submit once. Use proved fixed selectors, never generic clicks. FileList assignment alone is not upload completion. Preserve existing text/image drafts; close owned tabs and discard transfers on failure. Governs R1, R2, R5, R6.
- KTD6. Check journal before downloading. Record image-send intent before the click-capable operation, then confirmed conversation/message evidence. Reconcile ambiguous/crashed intent read-only when uniquely identifiable; otherwise return uncertainty without replay. For image asks, journal write/read failure blocks sending; loadJournal must retain intents without conversation IDs and never prune unresolved intents until terminal status is known. Text-only behavior stays unchanged. Retry a missing remembered conversation only on explicit no-submit evidence, with a fresh transfer. Governs R6.
- KTD7. Bind confirmation/anchoring to input-file evidence where available: ChatGPT/Claude parse human-image metadata. Other sites need proved readiness plus submitted-turn evidence. If text-only submission cannot be distinguished from success, leave the site disabled. Governs R1, R8.

### Per-site Evidence and Initial Disposition

No current site has input-upload operations or selectors. These are code findings, not product incapability claims. U1 must resolve every row with live evidence; no upload was attempted in this research-only pass.

| Site | Code evidence | Planned gate |
|---|---|---|
| ChatGPT | internal/history/chatgpt.go parses prompt image pointers/metadata; fixed composer in extension/send.js | Required: prove file control, readiness and submitted-image evidence |
| claude.ai | internal/history/claudeai.go parses human Files/FilesV2 images; ProseMirror composer | Next candidate; prove the same flow or record concrete deferral |
| Grok | internal/history/grok.go images are generatedImageUrls; ProseMirror composer | Input unproven; test file control and submitted-turn evidence |
| Gemini | internal/history/gemini.go/file operations read reply images; Quill composer | Input unproven; prove readiness independently of output capture |
| Perplexity | internal/history/perplexity.go recognizes image/attachment source flags; no file operation | Flags do not prove upload; test ask-input composer |
| Copilot | internal/history/copilot.go uses noFiles for output; extension/send.js gates personal accounts | Output limitation says nothing about input; test composer/readiness |
| Dots | internal/history/dots.go retains attachment counts; feed confirms owner messages | Prove DM upload separately, match attachment count and preserve pause/draft guards |

### Assumptions

- Proposed limits are Tincan limits, not vendor promises; use lower site/account limits when proved.
- File/DataTransfer plus change events in the isolated world is a hypothesis requiring live proof.
- Aborting composer upload cannot guarantee deletion from the vendor. Failure wording distinguishes upload from message submission.

## Implementation Units

### U1. Prove composer flows and freeze capabilities

- **Goal:** resolve the seven-site matrix before building around assumed upload mechanics.
- **Requirements:** R1, R5, R8; KTD1, KTD5, KTD7.
- **Dependencies:** none.
- **Files:** `extension/send.js`, `extension/test/send.test.js`, `internal/history/sites.go`, `docs/adapters/web-agents.md`; reader tests for added confirmation evidence.
- **Approach:** during implementation, test non-sensitive PNG/JPEG fixtures on all seven sites. Record fixed file-control/menu selectors, ready/error states, submitted-image evidence and account limits. ChatGPT must pass. Enable other sites only with the same proof; otherwise document the concrete deferral. Do not infer infeasibility from missing output file operations.
- **Patterns to follow:** SELECTORS/SITES and existing signed-in, anti-bot, paused-dot and keepDraft guards.
- **Test scenarios:**
  - Image plus text reaches new and existing ChatGPT conversations; the answer identifies visible fixture content.
  - Each other site passes readiness/submission proof or gets a reproducible deferral reason.
  - Two images arrive in order; rejection of the second prevents text submission.
  - Existing text/image drafts survive; missing grant, sign-out, challenge and paused dot prevent upload/send.
- **Verification:** retain DOM fixtures and live results in the guide; mocked FileList assignment is insufficient. Failed ChatGPT proof blocks release.

### U2. Bounded transfer and composer submission

- **Goal:** deliver bytes without arbitrary networking or scripting authority.
- **Requirements:** R1, R3, R5 to R8; KTD1, KTD3 to KTD5, KTD7.
- **Dependencies:** U1.
- **Files:** `internal/history/native.go`, `internal/history/sites.go`, `internal/history/native_test.go`, `extension/ops.js`, `extension/send.js`, `extension/background.js`, `extension/manifest.json`, `extension/test/ops.test.js`, `extension/test/send.test.js`, `extension/test/background.test.js`.
- **Approach:** add capability negotiation and fixed transfer operations. Bind tokens to one connection/site, enforce KTD3 budgets and cleanup/reload rules, then consume once inside the queued send. Use dedicated validated transfer payloads: existing OpArgs uses struct-equality checks, so a slice cannot simply be added. Fixed page code reconstructs Files and waits for proved ready/error states before text fill and submission. Keep output downloads separate.
- **Patterns to follow:** SPEC/errorFrame, native framing caps, emitBytes sequencing as an inverse-direction reference, createSender queues and owned-tab cleanup.
- **Test scenarios:**
  - Maximum chunk and escaped metadata fit both existing frame caps.
  - Bad base64, sequence gaps/duplicates, wrong hash/size, cross-site/connection tokens and unknown arguments fail closed.
  - Count, per-file, request and worker memory limits hold under concurrency.
  - Abort, expiry, disconnect and reload release buffers and cannot replay consumed sends.
  - Old hello/host/extension rejects image asks before mutation; text-only sends still work.
  - All files become ready before text fill/click; upload error, timeout or logout prevents submission.
  - Drafts remain intact and per-site queued operations cannot overlap.
- **Verification:** native framing and Node tests, then live PNG/JPEG sends on every enabled site.

### U3. Authorized downloads and request lifecycle

- **Goal:** feed validated relay attachments into image sends with accurate failures and no replay.
- **Requirements:** R1 to R4, R6, R7; KTD2, KTD6, KTD7.
- **Dependencies:** U2.
- **Files:** `internal/history/web.go`, `internal/history/web_journal.go`, `internal/history/web_poll.go`, `internal/history/web_test.go`, `internal/history/native.go`; new `internal/history/web_input.go`, `internal/history/web_input_test.go`. Reuse `internal/client/attachments.go`.
- **Approach:** claim, authorize chain, validate thread, check journal, then reject unsupported metadata before fetching. Download all images within one budget, validate/stage, transfer and send under the existing mutex/request deadline. Record intent/confirmation and retain clicked uncertainty. Never use sender filenames as paths. Replies name unsupported files and suggest supported images or text alone.
- **Patterns to follow:** DownloadAttachment checks, ChainDenied, sendFailure, journal resume-before-send, attachImages directory cleanup.
- **Test scenarios:**
  - Allowed request fetches as recipient and sends exact bytes in attachment order.
  - Denied chains, held requests and disabled sites fetch nothing and touch no composer.
  - Missing/deleted/forbidden file, checksum/MIME mismatch, malformed image, excessive pixels and mixed PNG/PDF fail explicitly.
  - Four files, 10 MiB per file and 20 MiB aggregate accept exact limits and reject excess.
  - Unwritable/corrupt journals and new-chat intents lacking conversation IDs cannot bypass replay protection.
  - Journaled sends resume without fetch/upload; ambiguous intents and lost/post-click confirmation never auto-resend.
  - Remembered missing conversation retries only with no-submit proof and fresh transfer; explicit missing conversation fails.
  - Before-click failure distinguishes uploaded vendor files from unsent text.
  - Timeout, panic, cancellation and disconnect remove staging and transfer state.
- **Verification:** fake-native and attachment-enabled relay integration tests, race checks and repeated-request regression.

### U4. Instructions and regression coverage

- **Goal:** advertise only verified input capabilities and preserve output descriptions.
- **Requirements:** R8, R9, R7.
- **Dependencies:** U1 to U3.
- **Files:** `docs/adapters/web-agents.md`, `README.md`, `internal/onboard/templates/agent.tmpl`, `internal/onboard/web_test.go`, `internal/cli/web.go`, `docs/protocol.md`, `extension/README.md`; MCP tool prose in `internal/mcpserver/server.go` if needed.
- **Approach:** publish final enabled/deferred matrix, limits, vendor exposure and upgrade guidance. Show CLI/MCP meme attachment examples. Render input wording only for enabled kinds. Retain Claude/Copilot output limits and remote MCP path restrictions. Relay attachment APIs do not change.
- **Patterns to follow:** per-kind template branches, rendered internal/onboard/web_test.go assertions and adapter approval/grant sections.
- **Test scenarios:**
  - All seven rendered kinds match the matrix; enabled kinds explain attaching images.
  - Deferred/old-extension failure replies match documented limitations.
  - Text-only asks, generated images, source links and dot outbound text asks retain behavior.
- **Verification:** templates, complete Go/extension suites and live cropped-meme acceptance.

## Risks

- DOM and readiness signals change. Ambiguous signals fail closed; fixtures do not replace live acceptance.
- Files may reach the vendor before an aborted text submission; deletion cannot be promised.
- A crash after a click can lose confirmation. Intent journaling favors explicit uncertainty over duplicate messages.

## Verification Contract

Planning only: implementation and live tests have not been run for this document.

| Gate | Command or check | Proves |
|---|---|---|
| Suite | `go test ./...` | Relay fetch, validation, journaling, template and text-only regressions |
| Race | `go test -race ./internal/history ./internal/client ./internal/relay` | Request serialization, upload transport and cleanup |
| Vet | `go vet ./...` | Go static checks |
| Lint | `make lint` | No new findings; compare the supplied six pre-existing modernize issues during implementation |
| Linux | `GOOS=linux go build ./...` | Platform-independent native/client and relay build |
| Extension | `make extension-test` | Actual Makefile Node test command covers validation, transfer and composer state |
| Native boundary | Maximum-size framed messages, reconnect, expiry and reload tests | Existing 256 KiB service and 1 MiB host caps are respected |
| Live | Cropped meme plus text on ChatGPT; PNG/JPEG, multiple inputs and continued conversations on every enabled site | Image is visible to the model and uploaded before submit |
| Matrix | All seven rows resolved as enabled with proof or deferred with concrete evidence | No unsupported capability claims |
| Approval | Held dot request remains unprocessed, then approved request uploads only if dots is enabled | Existing approval and chain boundaries preserved |

## Definition of Done

- ChatGPT passes the cropped-meme case and identifies visible input content.
- Every enabled site has live upload/readiness/submission proof; deferred sites fail clearly with documented evidence.
- No invalid or partial input becomes a silent text-only send; exposure and uncertainty are accurately reported.
- Transfers respect native caps, accept no arbitrary URLs/code and clean up every termination path.
- Approval, chain, replay and text-only regressions pass; docs and instructions match the final matrix.
- Gates pass except the existing lint baseline; this PR needs no signed-out-status work.
