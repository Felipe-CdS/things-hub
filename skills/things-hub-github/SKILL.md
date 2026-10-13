---
name: things-hub-github
description: Collect the owner's personal GitHub obligations (review requests, assigned issues and PRs, own PRs needing a fix) as things-hub events and post them to the inbox. Use as step 1 of a things-hub run (RUN.md), on the owner's Mac, with the Mac's existing gh login.
---

# things-hub GitHub collector (personal account)

You turn what the owner has to do on GitHub into things-hub events and post them with `thub post`. Read-only at GitHub: never comment, review, push, merge, label or change anything there. Only `gh search`, `gh pr view`, `gh issue view` and `gh api` GET calls.

## Scope

- **Only repositories owned by the personal account**: `THUB_GITHUB_LOGIN` from the `.env` (`felipe-cds`). Never an org, never someone else's repository. `thub check-batch` refuses any event whose `source_id` owner is not that login.
- The `account` of every event is that login, lowercased. Take it from the `.env` (`thub cursor github` prints it), never guess it.
- Mentions are out of scope for now.

## Steps

Work in the run folder the run uses (`$RUN`, see RUN.md). Let `LOGIN` be the login.

1. **Review requests** → kind `review-requested`:

   ```sh
   gh search prs --review-requested=@me --owner "$LOGIN" --state open --archived=false \
     --json number,title,url,updatedAt,repository,author --limit 100
   ```

   `revision` = the PR's `updatedAt`. Title: `Review PR: <PR title> (<repo>#<n>)`.

2. **Assigned issues and PRs** → kind `assigned`:

   ```sh
   gh search issues --assignee=@me --owner "$LOGIN" --state open --archived=false --include-prs \
     --json number,title,url,updatedAt,repository,isPullRequest --limit 100
   ```

   `revision` = `updatedAt`. Title: `Issue: <title> (<repo>#<n>)` or `PR: <title> (<repo>#<n>)`.

3. **Own PRs needing a fix** → kind `pr-needs-fix`:

   ```sh
   gh search prs --author=@me --owner "$LOGIN" --state open --archived=false \
     --json number,repository --limit 100
   # then, for each one:
   gh pr view <n> -R <owner/repo> --json title,url,reviewDecision,latestReviews,statusCheckRollup,updatedAt
   ```

   It needs a fix when `reviewDecision` is `CHANGES_REQUESTED`, or a check in `statusCheckRollup` has conclusion `FAILURE`, `TIMED_OUT`, `CANCELLED`, `ACTION_REQUIRED` or `STARTUP_FAILURE` (or a status context with state `FAILURE` or `ERROR`). `revision` = the time of what made it need a fix: the latest `CHANGES_REQUESTED` review's `submittedAt`, or the latest failing check's `completedAt`, whichever is newer. Title: `Fix PR: <title> (<repo>#<n>)`; the summary says which (changes requested by whom, or which checks fail).

4. **Resolve what left the sets.** `thub open github` lists the items still open on the inbox for this account. For each open key you did not find in steps 1 to 3 under the same kind, send a `resolve` event with the same `kind` and `source_id`, `revision` = now (RFC 3339 UTC), and no title or url. This is what completes the Things to-do when a PR is merged, a review is submitted, or an issue is closed.

5. **Write and post** `$RUN/github-batch.json`, then:

   ```sh
   thub check-batch "$RUN/github-batch.json"   # fix the file until it passes
   thub post "$RUN/github-batch.json"
   ```

   `post` prints per-event results: `accepted` (new or changed), `duplicate` (already known, harmless), `stale`. If the inbox is unreachable it keeps the batch in the spool; say so in the run report, and `thub flush` resends it later.

## Event shape

One batch, envelope `things-hub.batch.v1`, at most 200 events (split larger sets into several files):

```json
{
  "schema": "things-hub.batch.v1",
  "events": [
    {
      "source": "github", "account": "felipe-cds", "kind": "review-requested",
      "source_id": "Felipe-CdS/mic#12", "op": "upsert",
      "revision": "2026-10-13T11:50:02Z", "observed_at": "2026-10-13T12:00:00Z",
      "url": "https://github.com/Felipe-CdS/mic/pull/12",
      "title": "Review PR: microcode loader (mic#12)",
      "summary": "Ana requested your review on the microcode loader change.",
      "from": "ana", "labels": { "repo": "Felipe-CdS/mic", "type": "pr" }
    }
  ]
}
```

- `source_id` is `repository.nameWithOwner` + `#` + number, exactly as GitHub returns it.
- `title` at most 140 characters, imperative. `summary` at most 1000 characters: what is asked and by whom; never paste PR bodies or diffs.
- `observed_at` is now. Unknown fields are refused.
- The server computes the key `github:<login>:<kind>:<source_id>`; never build it yourself.
