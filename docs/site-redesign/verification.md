# Redesign verification — October 21, 2026

Release state: preview only. Production has not been promoted.

Latest preview: https://agenttincan-82gyo94po-mvanhorns-projects.vercel.app (Vercel authentication required). Deployment `dpl_6ew5nNFM8C3qd1gRacFAVKYABgC7`.

## Verified

- `npm --prefix tests/site test`: passes exact setup prompt preservation, stable anchors, local anchor destinations, unchanged privacy and machine endpoints/security configuration, four complete example transcripts, forward/reverse scroll selection, motion preference, blocked storage, denied clipboard fallback, disclosure Escape handling and theme controls. Added regressions for stable viewport height and restoring the selected transcript when re-enabling motion.
- `node --check site/script.js`, `node --check site/motion.js`, and `git diff --check`: pass.
- Native Safari desktop: generated hero cans, header stars, phone composition and request scene inspected. Chrome Canary responsive: 320px and 440px phone compositions inspected; forward scrolling advances examples through Second opinion; motion-off retains the selected full transcript. Live GitHub response displayed 334 stars.
- Image review: all three generated images and the social card inspected. The phone image is transparent outside its frame.
- Original deployment restored in a protected Vercel preview. `/agents.txt`, `/llms.txt`, and `/install.sh` compare byte-for-byte with baseline; homepage differs only by Vercel’s injected preview toolbar.
- No runtime framework or animation dependency added. Generated WebP artwork totals 216,112 bytes; later demo artwork is lazy-loaded. The launch video uses muted autoplay, looping, native controls and `preload=auto`. JS/CSS total 25,514 bytes gzip across the ordered modules, below the 200KB budget. These are asset sizes, not real-user performance metrics.

The launch film is now embedded in the opening hero, beside the introduction on desktop and below it on mobile, replacing the decorative hero cans. Native controls expose playback and sound directly; the lower-page collapsed film was removed. Focused tests pass after this change; its final visual check remains blocked by unavailable browser windows.

## Review and corrections

`ce-code-review` completed with a Claude cross-model adversarial pass (requested/reported `claude-opus-5-5`, requested high effort, actual effort unverified). Local persona and validation passes ran sequentially in the main thread per the user’s AGENTS instructions, so they are not independent coverage. Run: `tincan-20261021`, local receipt `/tmp/compound-engineering-501/ce-code-review/tincan-20261021/review.json`.

All seven retained findings received fixes: stable small-viewport probe; ordered CSS modules; remeasurement after disclosure/content resizing; transcript position restoration; nonoverlapping static Council; opaque no-JS header; compact caption reset. Added cached last-known GitHub count. Short desktop scenes use compact layout through 820px height, and tablet demo uses the vertical composition through 899px width.

## Release gates and coverage limits

Final browser recheck was interrupted when computer control began returning `noWindowsAvailable`; the browser inventory subsequently contained no windows or connected browser surfaces. Therefore the final CSS module split and review corrections have automated/source checks but need a fresh visual pass.

Before production promotion: inspect final preview at 320/390/768/1440px and 200% zoom; inspect short landscape, all three scenes forward/backward, reduced-motion and missing-image states, and disclosure-induced geometry changes. Check physical iOS Safari/Android Chrome toolbar behavior. Capture a motion recording and run a documented throttled mobile performance profile for LCP <=2.5s, CLS <=0.1 and animation long tasks. Those measurements and recordings are not yet available; no passing claim is made.

## Repository-wide checks

`make test` failed in unchanged backend packages with timeouts/timing failures (including listen ping, Council, History, relay/store/wake). `make lint` reported nine existing Go modernize warnings. `make extension-test` failed an unchanged permissions assertion in `extension/test/background.test.js:147`. The redesign does not modify those files. These runs do not establish a green full-repository baseline; their failures are separate from the passing focused site checks.

Before promotion, recheck the production deployment and Instant Rollback eligibility as described in [rollback.md](rollback.md). Preview restoration proves artifact recovery, not a production rollback SLA.

Deployed preview smoke checks: demo CSS and agents.txt match the local artifact exactly; `/privacy` resolves. Final browser validation is still pending.
