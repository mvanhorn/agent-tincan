# Release baseline

Captured October 21, 2026 before site changes. Production was byte-identical to the static files in main at `cce368f6377e02dbd71ba540e2e62c53fa3d6325`.

- Source checkpoint: `site-before-cinematic-20261021` (local Git tag).
- Production deployment: `dpl_DNdSWXF8pUgkVPjuvg3JL33RBxiF`.
- Immutable production URL: https://agenttincan-8zj96zhhp-mvanhorns-projects.vercel.app
- Project/scope: `agenttincan` / `mvanhorns-projects`.
- Local artifact: `/Users/mvanhorn/agent-tincan-site-backup-20261021/site.tar.gz`.
- Checksums: `/Users/mvanhorn/agent-tincan-site-backup-20261021/manifest.json`.
- Preserved Vercel project configuration: root directory `site`; static site, existing `site/vercel.json` unchanged.

The baseline was successfully redeployed without changing production:
https://agenttincan-p5s1tur33-mvanhorns-projects.vercel.app

Restored deployment: `dpl_6UozS5GtLKq9gADa1JQFjKQBZjro` (READY). Its homepage matches the preserved HTML, with only Vercel's preview-toolbar script appended. Preview authentication remains enabled. Initial attempts from the `site` subdirectory failed because Vercel expected a parent containing `site`; deploying that parent succeeded.
