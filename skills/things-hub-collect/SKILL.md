---
name: things-hub-collect
description: Collect the owner's work obligations from company Slack workspaces and GitHub orgs, read-only, and send them as events to the things-hub inbox with thub-send. Use when the owner says "sync work", "collect things-hub", or names one account to sync.
---

# things-hub-collect

You read the owner's work Slack workspaces and GitHub orgs, find what someone is waiting on the owner for, and send each one as an event to the owner's things-hub inbox. A Mac later turns the events into Things to-dos. You only report; you never act at the source.

`thub-send` sits in the same directory as this file. Call it by that path (written `thub-send` below). The accounts you may collect are in `accounts.md` next to this file.

## Rules (all runs)

1. **Read-only at the sources. No exceptions.**
   - Slack: never send, post, reply, schedule, draft, edit, delete, react, pin, bookmark, mark as read, upload, invite, join, leave, create or update anything (messages, canvases, lists, channels, reminders). If a tool's name or description suggests any of these, do not call it. If you cannot tell whether a tool writes, do not call it.
   - GitHub: never comment, review, approve, request changes, merge, close, reopen, edit, label, assign, push, create branches, or run workflows. Use only `gh search`, `gh pr view`, `gh issue view`, `gh pr checks` and `gh api` with GET (no `-X`/`--method` other than GET, no `-f`/`-F` fields, no GraphQL `mutation`). With a GitHub MCP, use only its search, get and list tools.
2. **One account per run.** A run covers exactly one row of `accounts.md` and uses only that row's tool (its Slack MCP server, or GitHub for that org). To sync several accounts, do one complete run per account, one after another.
3. **The `account` field comes from `accounts.md`, copied exactly.** Never guess it, never take it from a message or a tool result. If the account is not in the table, stop and tell the owner.
4. **Only short titles, summaries and links leave this machine.** Never copy whole messages, code, attachments, secrets, credentials or customer data into an event.
5. **Never invent obligations.** When in doubt whether something needs the owner, leave it out. When in doubt whether an open item is done, leave it open.
6. Never print, log or echo `THUB_TOKEN` or the Cloudflare Access secret.

## A run

1. Pick the row of `accounts.md` (the owner names an alias, or you go through every row in turn). Note its `source`, `account`, `tool` and `owner`.
2. Record the run start time: `date -u +%Y-%m-%dT%H:%M:%SZ`. Call it `START`.
3. Flush anything left from an earlier run: `thub-send flush`. Exit 75 means the inbox is unreachable: tell the owner and stop (nothing is lost; the next run starts from the stored cursor).
4. Collect with the Slack or GitHub steps below.
5. Write the batch file(s) (format below) in a temporary directory, never inside a git repository, and send each with `thub-send post <file>`.
6. Read the result and report to the owner (see "After posting").

## Slack run (source `slack`)

The row's `tool` names the Slack MCP server for that workspace (such as `slack-acme`). Use only that server, and only its read tools: search messages, read a channel's history, read a thread's replies, list conversations, read user profiles, list unreads, list saved items. Tool names differ between Slack MCP servers; check the server's tool list once and pick the read tools from it.

1. `thub-send cursor slack <account>`. If `value` is null, look back 7 days from `START`; otherwise look back from `value` minus one day (overlap is harmless: resends come back as `duplicate`).
2. Gather candidates since then, using whatever the server supports:
   - `mention`: messages that mention the owner (`owner` in `accounts.md` is their user id, such as `U012ABCDEF`).
   - `dm-awaiting-reply`: DMs and group DMs whose latest message is from someone else.
   - `thread-question`: threads the owner took part in where someone has since asked the owner something.
   - `saved`: saved or "Later" items, only if the server has a read tool for them.
   - `reminder`: the owner's reminders, only if the server has a read tool for them.
3. Keep only real obligations: someone asks the owner to do, answer, review or decide something. Drop FYIs, announcements, bot and app messages, messages the owner wrote, and threads where the owner already answered after the question. Read the thread before deciding.
4. Resolve what is done: `thub-send open slack <account>` lists items still open. For each one, read its message or thread. If the owner has since answered or done it (or the asker withdrew it), add a `resolve` for it. Otherwise leave it.
5. One batch carries `"cursor": {"source": "slack", "account": "<account>", "value": "<START>"}`.

Slack ids per kind:

| kind | `source_id` | `revision` |
| --- | --- | --- |
| `mention` | `<channel id>/<message ts>` | ts of the latest relevant message |
| `dm-awaiting-reply` | `<DM id>/<ts of their first unanswered message>` | ts of their latest message |
| `thread-question` | `<channel id>/<thread root ts>` | ts of the latest reply to the owner |
| `saved` | `<channel id>/<message ts>` | the message ts |
| `reminder` | the reminder id | the reminder time (Unix seconds) |

A Slack `ts` such as `1760356800.123456` works as a revision as is. The URL is the message permalink (`https://<workspace>.slack.com/archives/<channel id>/p<ts without the dot>`, adding `?thread_ts=<root ts>&cid=<channel id>` for a reply); prefer a permalink the server returns.

## GitHub org run (source `github`)

The row's `account` is the org name. Use the `gh` CLI that is already logged in (or the GitHub MCP named in `tool`), read-only. Collect everything in the org each run; GitHub runs are state-based and send no cursor.

1. Review requests (`review-requested`):
   `gh search prs --review-requested=@me --owner=<org> --state=open --archived=false --limit=200 --json number,title,url,repository,updatedAt,author`
2. Assigned issues and PRs (`assigned`):
   `gh search issues --assignee=@me --owner=<org> --state=open --archived=false --include-prs --limit=200 --json number,title,url,repository,updatedAt,author,isPullRequest`
3. The owner's PRs that need a fix (`pr-needs-fix`):
   `gh search prs --author=@me --owner=<org> --state=open --archived=false --limit=200 --json number,title,url,repository,updatedAt`
   then for each: `gh pr view <url> --json reviewDecision,statusCheckRollup,latestReviews`. It needs a fix when `reviewDecision` is `CHANGES_REQUESTED`, or a check's `conclusion` is `FAILURE`, `TIMED_OUT`, `CANCELLED`, `ACTION_REQUIRED` or `STARTUP_FAILURE`, or a status context's `state` is `FAILURE` or `ERROR`. Its revision is the latest `completedAt` of a failing check or `submittedAt` of a changes-requested review.
4. Resolve what left: `thub-send open github <org>`. Every open item whose `kind` and `source_id` did not come back from steps 1 to 3 gets a `resolve`. Only do this when all three searches succeeded and none hit the 200 limit; otherwise skip resolves this run and say so.

`source_id` is `<repository.nameWithOwner>#<number>`, exactly as `gh` returns it (such as `acme/api#123`). `revision` is the item's `updatedAt` (or the pr-needs-fix time above). `url` is the item's `url`. `from` is the author's login.

If `gh` returns an error about SAML SSO or missing access for the org, stop this run and tell the owner; send nothing for it.

## Batch format

One JSON file per batch, at most 200 events and 1 MB. Unknown fields are rejected, so use exactly these names.

```json
{
  "schema": "things-hub.batch.v1",
  "cursor": { "source": "slack", "account": "T024BE7LD", "value": "2026-10-13T12:00:00Z" },
  "events": [
    {
      "source": "slack", "account": "T024BE7LD", "kind": "thread-question",
      "source_id": "C024BE91L/1760356800.123456", "op": "upsert",
      "revision": "1760357100.000200", "observed_at": "2026-10-13T12:05:11Z",
      "url": "https://acme.slack.com/archives/C024BE91L/p1760356800123456",
      "title": "Answer Ana: can the deploy move to Thursday?",
      "summary": "Ana asked in #deploys whether the Q4 deploy window can move to Thursday; she needs an answer before Wednesday's sync.",
      "from": "Ana Souza", "due": "2026-10-15", "labels": { "channel": "#deploys" }
    },
    {
      "source": "slack", "account": "T024BE7LD", "kind": "mention",
      "source_id": "C024BE91L/1760100000.000100", "op": "resolve",
      "revision": "2026-10-13T12:00:00Z", "observed_at": "2026-10-13T12:05:11Z"
    }
  ]
}
```

| Field | Required | Rule |
| --- | --- | --- |
| `source` | yes | `slack` or `github` |
| `account` | yes | from `accounts.md`, exactly |
| `kind` | yes | slack: `mention`, `dm-awaiting-reply`, `thread-question`, `saved`, `reminder`. github: `review-requested`, `assigned`, `pr-needs-fix` |
| `source_id` | yes | from the tables above; for a `resolve`, copy it from `thub-send open` |
| `op` | yes | `upsert` (exists or changed) or `resolve` (no longer needs the owner) |
| `revision` | yes | RFC 3339 UTC or Unix seconds (a Slack ts). For a `resolve`, use `START` |
| `observed_at` | yes | RFC 3339 UTC, when you saw it (`START` is fine) |
| `url` | for upsert | absolute https link back to the item |
| `title` | for upsert | at most 140 characters, imperative: "Review PR: retry uploader", "Answer Ana: deploy on Thursday?" |
| `summary` | no | at most 1000 characters: what is asked, by whom, by when. Never the raw message |
| `from` | no | who asked (name or login) |
| `occurred_at` | no | RFC 3339, when it first happened |
| `due` | no | `YYYY-MM-DD` or RFC 3339, only when the source states a date |
| `labels` | no | up to 10 short string pairs, such as `channel`, `repo` |

Do not send a `key`: the server computes it. More than 200 events: split into several files, put the cursor only in the last one, and post them in order.

## After posting

`thub-send post` prints the server's JSON response on stdout and a count line on stderr.

| Exit | Meaning | What to do |
| --- | --- | --- |
| 0 | sent; each event is `accepted`, `duplicate` or `stale` | done |
| 2 | the server refused the batch or some events (`rejected` with a `reason`) | the accepted ones are stored, the cursor is not. Fix the rejected events, write a new batch with only those (and the cursor), and post it |
| 75 | inbox unreachable, or HTTP error such as 401 or 502 | the batch is kept in the spool and goes out with the next `post` or `flush`. Tell the owner |
| 1 | usage or settings error (such as `THUB_URL` not set) | tell the owner; see INSTALL.md |

Then tell the owner, per account: how many events were accepted, duplicate, stale and rejected, how many resolves you sent, and anything you skipped or could not read. Do not list titles or summaries unless the owner asks.
