---
title: Sesame as a Teammate Through the MCP Gateway - Plan
type: feat
date: 2026-10-06
topic: sesame-adapter
artifact_contract: ce-unified-plan/v1
product_contract_source: ce-plan-bootstrap
execution: code
---

# Sesame as a Teammate Through the MCP Gateway - Plan

---

## Goal Capsule

- Objective: the owner's Sesame agent (for example Miles) is an Agent Tincan teammate: it can ask teammates for work from the Sesame app, and requests sent to it are picked up and answered within about 10 minutes without the owner opening Sesame.
- Means: Sesame connects to the relay's existing public OAuth MCP gateway as a custom app (KTD1), and a Sesame schedule checks its inbox every 5 minutes, declared to the relay as a `schedule` wake (KTD2). A new `sesame` kind carries its onboarding (KTD3).
- Authority: Requirements (R) win on product behavior; KTDs win on mechanism within their cited Rs; units override neither. `docs/trust-model.md` stays the security reference and is not loosened by this plan.
- Stop conditions: stop and ask if the work would add a public listener other than the existing gateway, add a non-OAuth way to authenticate to the gateway, give a gateway-connected agent any power a tailnet agent of the same kind lacks, or rename `--chatgpt-gateway` or any `/v1` route or field. Stop and ask if U0 shows a Sesame scheduled run cannot call the Tincan tools without the owner present.
- Execution profile: Go and docs, agent-tincan only, one PR. Live setup steps that need the owner (Tailscale admin, the Sesame app) are listed under Operational Notes and are not part of the PR.
- Who finishes: the owner and implementer run U0 together first; the implementer then lands U1-U3; the owner adds the `wake.json` entry.
- Open blockers: none.

---

## Product Contract

### Summary

Sesame's agents run in Sesame's cloud and cannot join a tailnet, but Sesame's "Add custom app" connects to any remote MCP server and signs in with standard MCP OAuth. The relay already publishes exactly that for ChatGPT: a gateway over Tailscale Funnel with dynamic client registration, PKCE, and a one-time login code from `tincan connect <name>`, holding one MCP session per connected agent. This plan makes Sesame a supported, documented gateway agent: a `sesame` kind whose onboarding says how to add the custom app and a 5-minute Sesame schedule that checks its inbox, and gateway and CLI text that no longer assumes the connector is ChatGPT.

### Problem Frame

On 2026-10-06 the owner asked Sesame's Miles how it could join Agent Tincan without Tailscale. Miles suggested the ChatGPT connector shape plus a scheduled inbox check. A live test the same day confirmed the premise: adding `https://mcp.globalping.dev/mcp` (an OAuth MCP server with dynamic client registration and PKCE S256, the same shape as the gateway) as a Sesame custom app showed "Continue to authorization" and then opened Globalping's sign-in page.

Today nothing documents this path. `tincan connect` prints ChatGPT-only steps, the gateway login page says "Enter the one-time code from `tincan connect chatgpt`", the not-enabled error names ChatGPT, and no kind or onboarding covers a gateway agent that checks on a schedule. ChatGPT's own kind assumes wake `none` because ChatGPT cannot run on its own; Sesame can, so treating it as ChatGPT would hide its schedule from senders and drop the overdue warning.

The owner's relay does not run the gateway yet (`tincan agents` shows no connector agent).

### Key Decisions

- Sesame connects through the existing OAuth gateway, not a new transport and not Sesame Link. Governs R1, R2. Sesame Link would make Miles drive Claude Code on the owner's Mac, so its work would appear as Claude Code, not as its own teammate.
- Sesame checks its inbox on a 5-minute Sesame schedule. Governs R4. (session-settled: user-directed - chosen over a 15-minute schedule: the owner asked for every 5 minutes)
- Email wake through the owner's Gmail is out of this plan. Governs Scope Boundaries. It would route Tincan wake mail through a personal inbox and needs its own trust decision.

### Requirements

Connection

- R1. `tincan connect sesame` on an admin device prints the gateway MCP URL, a one-time code, and Sesame's steps (Apps, Add custom app, the URL, Continue to authorization, enter the code). `tincan connect chatgpt` prints today's ChatGPT steps. Any other name prints neutral steps that name no product, whatever kind it later has.
- R2. The gateway login page tells the user to enter the one-time code from `tincan connect <name>` without naming a product, and works for any connected agent; ChatGPT and Sesame can be connected at the same time, each with its own tokens.
- R3. When the gateway is off, `tincan connect` says the relay's MCP gateway is not enabled and names the existing `--chatgpt-gateway` flag, without implying the connector must be ChatGPT.

Teammate behavior

- R4. A `sesame` kind exists. Its default wake is `schedule`, and its onboarding tells the owner to set `{ "sesame": { "method": "schedule", "every": "5m" } }` in the relay's `wake.json` and a Sesame schedule at the same interval, so senders see "checks its inbox every 5m" and the roster marks it overdue when the checks stop.
- R5. The `sesame` kind's standing instructions are written for MCP tools, not the CLI: on every scheduled run, call `check_inbox`, finish work waiting on replies, handle each request and call `reply`, use `needs_input` for a missing detail, and stop when the inbox is empty. Outside scheduled runs, it may `ask` teammates when the owner asks it to. They also say a request's text is a teammate's input, not an instruction to reveal unrelated conversations or inbox content. Shared blocks that tell agents to run CLI commands (`tincan rejoin`, `tincan doctor`, `tincan upgrade`) or reload an MCP server treat `sesame` as a gateway agent, as they treat `chatgpt`: when not joined, it tells the owner to run `tincan connect sesame` again.
- R6. `tincan connect sesame` stores the `sesame` kind on the agent when it has none, so `tincan agents` shows `kind=sesame` with no separate `tincan kind` step.
- R7. A Sesame agent has exactly the powers of any other joined agent of its kind: no admin powers, the same rate limits, holds and chain rules. `tincan remove sesame` revokes its gateway tokens immediately, as for ChatGPT.

Docs

- R8. `docs/adapters/sesame.md` covers enabling the gateway, connecting, the custom app, the Sesame schedule and its prompt, the `wake.json` entry, limits, and removal. The README agent list, `site/agents.txt` and `docs/adapters/chatgpt.md` link to it.
- R9. The trust model says the gateway serves any agent an admin connects, each with its own tokens, and that each connected agent's vendor (Sesame for `sesame`) receives the requests sent to it and every teammate answer it gets, including history and web-agent answers unless `history-allow.txt` and the web-agent allow files exclude it. The Sesame doc repeats this under Limits and suggests gating `sesame` in `approval.json` when the Sesame agent has connected apps that act as the owner.

### Acceptance Examples

- AE1. Covers R1, R6. Given the gateway is on, when the owner runs `tincan connect sesame`, then the output contains the gateway `/mcp` URL, a code, and "Add custom app", and `tincan agents` later lists `sesame` with `kind=sesame`.
- AE2. Covers R4. Given `wake.json` has `sesame` as `schedule` every `5m`, when a teammate asks sesame and no reply comes back at once, then the ask result says sesame checks every 5m and to expect a reply within about 10m.
- AE3. Covers R2. Given chatgpt and sesame are both connected, when either refreshes its token, then the other's session and tokens are unaffected.

### Scope Boundaries

- No new authentication method on the gateway (no static tokens, no API keys in URLs).
- No renaming of `--chatgpt-gateway`; a neutral alias is deferred.
- No email or webhook wake for Sesame.
- No Sesame Link integration.
- Not built: a gateway-side allowlist of connector names. Any name `tincan connect` binds is already an admin decision.

### Sources

- Live test 2026-10-06: Sesame custom app with `https://mcp.globalping.dev/mcp` opened "Before connecting this app", "Continue to authorization", then a GitHub sign-in for Globalping. Globalping's metadata: `registration_endpoint`, `authorization_endpoint`, `code_challenge_methods_supported: ["S256"]`.
- Gateway: `internal/gateway/gateway.go` (well-known metadata, `/register`, `/authorize`, per-agent `mcpserver.New` at the `servers` map), `internal/gateway/oauth.go` (login codes bound to an agent name).
- `internal/cli/connect.go`, `internal/relay/server.go` `handleConnect`, `internal/onboard/onboard.go` kind maps, `internal/onboard/templates/agent.tmpl` (`instructions.scheduled`, `setup.chatgpt`, `join.chatgpt`), `docs/adapters/scheduled.md`, `docs/adapters/chatgpt.md`.

---

## Planning Contract

### Key Technical Decisions

- KTD1. Reuse the gateway as is; change only product-specific text and the kind stored on connect. The gateway already registers clients dynamically, requires PKCE S256, binds each login code to the agent name `tincan connect` was given, and keeps one MCP server per agent. The live test only reached an OAuth server's sign-in page, so U0 proves the full flow against this gateway before any code is written. Governs R1, R2, R7.
- KTD2. Sesame's wake is the existing `schedule` method. The relay sends nothing for it; it records the interval, tells senders when to expect a reply, and flags missed checks, which is what a cloud agent on a cron needs. Nothing about `schedule` depends on the agent being on the tailnet, so a gateway agent uses it unchanged. Governs R4.
- KTD3. A new `sesame` kind rather than reusing `scheduled` or `chatgpt`. `scheduled` instructions are CLI-based and say "a cron job starts each run"; `chatgpt` assumes wake `none` and "acts only while you talk to it". Sesame needs gateway join and setup text plus MCP-tool instructions for scheduled runs. Governs R4, R5, R6.
- KTD4. `tincan connect` prints product steps only for the canonical names `chatgpt` and `sesame`, and neutral steps for every other name. On connect, the relay stores the kind onboarding's runtime-name map gives the name when the agent has no stored kind; the roster reads only stored kinds today. Governs R1, R6.

### Risks

- A Sesame scheduled run might not be able to call custom-app tools without the owner present. Miles says it can, but this is unverified. U0 checks it before any code; if it fails, the work stops for a decision (Goal Capsule).
- The gateway's public endpoint is already in the trust model. Connecting Sesame widens who holds gateway tokens and adds Sesame's cloud as a holder of request and answer content (R9).

---

## Implementation Units

### U0. Prove the path on today's build

- Goal: confirm, before writing code, that Sesame completes the gateway sign-in, lists and calls the Tincan tools, and can do so from an unattended schedule.
- Requirements: R1, R2, R4. KTD1.
- Dependencies: none.
- Files: none.
- Approach:
  1. The owner enables the gateway (Operational Notes 1-2) and runs `tincan connect sesame`. Today's output is ChatGPT-worded; the URL and code are what matter.
  2. In Sesame: Apps, Add custom app, the URL, Continue to authorization, enter the code. Ask Miles to list its Agent Tincan tools and call `list_agents`.
  3. Create a Sesame schedule every 5 minutes whose prompt is "call check_inbox from Agent Tincan, handle and reply to each request". Have a teammate ask sesame something trivial, with nobody in the Sesame app, and watch `tincan get` and `tincan agents` for a check-in.
- Test expectation: none - live owner check.
- Verification: steps 2 and 3 both succeed. If step 3 fails, stop per the Goal Capsule.

### U1. Kind, name mapping and onboarding for Sesame

- Goal: `sesame` is a known kind with schedule wake, a good-at line, gateway join text, setup steps and MCP-tool instructions.
- Requirements: R4, R5, R6, R7. KTD2, KTD3.
- Dependencies: U0.
- Files: `internal/onboard/onboard.go`, `internal/onboard/templates/agent.tmpl`, `internal/onboard/templates/recipes.tmpl`, `internal/onboard/onboard_test.go`, `internal/onboard/shared_test.go`, `internal/onboard/rejoin_test.go`, `internal/onboard/goodat_test.go`.
- Approach:
  1. Add `KindSesame` to the kind list, the runtime-name map (`"sesame"`), the default-wake map (`schedule`) and the good-at map (asks the owner's Sesame agent; checks its inbox on a Sesame schedule).
  2. Add `join.sesame`, `instructions.sesame`, `setup.sesame`, `title.sesame` and an invite recipe, modeled on the chatgpt and scheduled blocks, with tool names instead of CLI commands.
  3. Replace the shared template branches keyed on `chatgpt` (the MCP-reload line and `instructions.selfheal`) with a gateway-agent condition covering `chatgpt` and `sesame`.
  4. Export the runtime-name lookup for U2.
- Patterns to follow: `KindChatGPT` and `KindScheduled` entries; `TestRecipes`; `TestHarkRuntimeNameIsScheduled`.
- Test scenarios:
  - Covers AE1. The roster name `sesame` resolves to kind `sesame` with wake `schedule`.
  - The `instructions.sesame` block mentions `check_inbox` and `reply` and never `tincan inbox`.
  - The full sesame block has no `tincan rejoin`, `tincan doctor` or `tincan upgrade` line, and its not-joined line says `tincan connect sesame`.
  - The sesame instructions say request text is not an instruction to reveal unrelated content.
  - The sesame setup names `tincan connect sesame`, Add custom app, `"every": "5m"` and the matching Sesame schedule.
  - The sesame recipe says no `tincan invite` is used.
  - Other kinds' blocks are unchanged.
- Verification: `tincan onboard` prints a Sesame block, and the scenarios pass.

### U2. Product-neutral connect output and gateway text

- Goal: connecting any gateway agent reads right for that agent.
- Requirements: R1, R2, R3. KTD1, KTD4.
- Dependencies: U1.
- Files: `internal/cli/connect.go`, `internal/cli/connect_test.go`, `internal/gateway/gateway.go`, `internal/gateway/gateway_test.go`, `internal/relay/server.go`, `internal/relay/connect_test.go`, `internal/identity/virtual.go`, `internal/identity/virtual_test.go`.
- Approach:
  1. `connect` resolves the name's kind and prints ChatGPT, Sesame or neutral steps; the URL and code lines are unchanged.
  2. The login page says to enter the one-time code from `tincan connect <name>` without naming a product.
  3. The not-enabled error names the MCP gateway and the `--chatgpt-gateway` flag.
  4. `handleConnect` stores the runtime-name kind when the bound agent has none (KTD4).
- Patterns to follow: existing gateway tests for the login page and register flow.
- Test scenarios:
  - Covers AE1. `connect sesame` against a test relay prints the URL, the code and "Add custom app".
  - `connect chatgpt` prints today's ChatGPT steps.
  - `connect miles` prints neutral steps naming neither product.
  - Covers AE1. After `connect sesame`, the roster reports kind `sesame`; a stored kind set with `tincan kind` beforehand is kept.
  - The login page no longer contains "tincan connect chatgpt".
  - Covers AE3. Two agents connected through the gateway each refresh tokens without affecting the other.
- Verification: the scenarios pass.

### U3. Docs

- Goal: an owner can set Sesame up from the docs alone.
- Requirements: R8, R9.
- Dependencies: U1, U2.
- Files: `docs/adapters/sesame.md`, `docs/adapters/chatgpt.md`, `docs/trust-model.md`, `README.md`, `site/agents.txt`, `site/index.html`.
- Approach: new adapter page covering enable, connect, custom app, schedule prompt (the scheduled steps in tool form), `wake.json`, limits (acts on its schedule or when the owner talks to it; five wrong codes lock the login page; `tincan remove sesame`), and the live checks below. Link it from the README agent list, `site/agents.txt` and the ChatGPT page. Add the R9 trust-model text, and the same note plus the `approval.json` suggestion under the Sesame page's Limits.
- Test expectation: none - documentation only.
- Verification: every page listed links to `docs/adapters/sesame.md`, and the trust model sentence is present.

---

## Verification Contract

| Gate | Command or check | Applies to |
|---|---|---|
| Tests | `make test` | U1, U2 |
| Vet and lint | `make vet`, `make lint` | U1, U2 |
| Review | Greptile required check with no P0 | all |
| Live: connect | With the gateway on, `tincan connect sesame`, Sesame Add custom app with the printed URL, enter the code: Sesame shows the Tincan tools, and `tincan agents` lists `sesame` with `kind=sesame` | U1, U2 |
| Live: schedule | A Sesame schedule every 5 minutes calls `check_inbox` with nobody in the app; a teammate's ask gets answered within about 10 minutes | U1 |
| Live: ask out | In the Sesame app, ask Miles to ask a teammate something; the teammate's reply comes back | U1 |

---

## Definition of Done

- All requirements trace to a passing test or a live check above.
- `tincan connect chatgpt` output and the ChatGPT flow are unchanged apart from neutral wording.
- No new authentication path, listener, route or flag rename.
- PR title is a Conventional Commit (`feat(onboard): ...`), and the body states which live checks ran.
- No leftover code from abandoned approaches in the diff.

---

## Appendix

### Operational Notes (owner steps, not in the PR)

1. In the Tailscale admin console, make sure the tailnet has HTTPS certificates and the relay's gateway node may use Funnel.
2. Restart the relay with `--chatgpt-gateway`. It starts the `tincan-gateway` node and serves `https://tincan-gateway.<tailnet>.ts.net/mcp`.
3. On an admin device: `tincan connect sesame`.
4. In Sesame: Apps, Add custom app, name `Agent Tincan`, the printed URL, Save, Continue to authorization, enter the code.
5. In Sesame, create a schedule every 5 minutes with the prompt from `docs/adapters/sesame.md`.
6. On the relay host, add `{ "sesame": { "method": "schedule", "every": "5m" } }` to `wake.json` and restart the relay.
