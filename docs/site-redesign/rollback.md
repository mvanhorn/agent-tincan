# Restore the previous website

Production has not been changed by this branch. Review the redesign preview before promotion. Keep the previous production deployment and this artifact until the redesign is accepted.

## Before promotion

Run `vercel inspect agenttincan.com` and compare the deployment with `baseline.md`. If another release has happened, preserve that newer production first; do not roll over it with the old snapshot. Confirm the previous production deployment is available for Instant Rollback in the project dashboard. Eligibility is separate from the successful artifact restore test.

## Instant Rollback

In Vercel, open project `agenttincan`, select the recorded previous production deployment, and choose Instant Rollback. The captured baseline is `dpl_DNdSWXF8pUgkVPjuvg3JL33RBxiF`, URL `https://agenttincan-8zj96zhhp-mvanhorns-projects.vercel.app`.

Vercel's plan and current deployment history determine eligibility. Recheck at release time. Instant Rollback also disables automatic domain assignment; intentionally promote a corrected/reverted deployment to resume it. No production rollback was executed during this work, so recovery elapsed time is not measured.

## Artifact recovery if Instant Rollback is unavailable

The original static site was already restored successfully as a protected preview at https://agenttincan-p5s1tur33-mvanhorns-projects.vercel.app.

From `/Users/mvanhorn/agent-tincan-site-backup-20261021`, run:

```sh
vercel deploy --yes --prod
```

This command changes production. Use it only when restoring production is intended. Deploy from the parent containing `site`, because the project rootDirectory is `site`. Preserve the `.vercel/project.json` project binding. The tar archive plus baseline source SHA can reconstruct the same layout on another machine.

After restoration, verify apex and www, `/privacy`, `/agents.txt`, `/llms.txt`, and `/install.sh`. Compare text endpoints to the manifest and check copy controls. Revert the redesign commit(s) on the source branch with `git revert`; do not reset unrelated commits or discard other work.
