---
name: things-hub-classroom
description: Collect the owner's Google Classroom assignments (due dates, turned-in state), read-only, and send them as events to the things-hub inbox with thub-send. Run by the Mac's scheduled Classroom routine, or when the owner says "sync classroom".
---

# things-hub-classroom

You read the owner's Google Classroom through `bin/thub-classroom`, decide which assignments the owner still has to do, and send them as events to the owner's things-hub inbox. A later on-demand run turns the events into Things to-dos; a `resolve` lets it complete the to-do once the work is turned in. You only report; you never act in Classroom and never touch Things.

Run every command from the repository root, exactly as written here. `thub-send` is `skills/things-hub-collect/thub-send`. Both tools read their settings from the repository's `.env` (the routine sets `THUB_ENV_FILE`).

## Rules

1. **Read-only.** The only commands you run are `date -u +%Y-%m-%dT%H:%M:%SZ`, `bin/thub-classroom assignments`, and `skills/things-hub-collect/thub-send` with `flush`, `open classroom <account>` or `post <file>`. Do not run anything else, and never call Classroom, Google or the inbox any other way.
2. **The `account` comes from the `account` field of the `assignments` output, copied exactly.** Never guess it or take it from anywhere else.
3. **Only short titles, summaries and links leave the helper's output.** Never copy a whole description, attachments, grades of other people, or anything secret into an event.
4. **Never invent assignments or states.** Use the helper's `source_id`, `due`, `url` and `done` as they are, and derive `revision` only as step 5 says.
5. Never print `.env`, a token or a client secret.
6. Unattended runs never ask questions: when something blocks the run, stop and say why in the report.

## A run

1. `START`: run `date -u +%Y-%m-%dT%H:%M:%SZ`.
2. `skills/things-hub-collect/thub-send flush`. Exit 75 means the inbox is unreachable or erroring: report it and stop (nothing is lost; the next run sends everything again).
3. `bin/thub-classroom assignments`. On a non-zero exit, send nothing, and report the error. If it mentions the refresh token or `thub-classroom login`, the owner must log in again (`make classroom-login`). If it says access is blocked or disabled by an administrator, the school blocks the app (docs/classroom-setup.md).
   The output is `{"account", "fetched_at", "courses", "assignments": [...]}`. Each assignment has `source_id`, `course`, `section`, `title`, `description`, `url`, `work_type`, `max_points`, `due`, `created`, `revision`, `state`, `late`, `done` (true when it is turned in or returned) and `old` (true when it was due, or without a due date created, and neither it, the submission nor the course changed in the 14 days before the fetch; missing means false).
4. `skills/things-hub-collect/thub-send open classroom <account>` lists the items still open in the inbox (`kind`, `source_id` and `revision`).
5. Build the events:
   - **Not done** (`done` false) and open: send an `upsert` with `revision` set to the later of the assignment's `revision` and the open item's `revision`; an unchanged one comes back `duplicate`.
   - **Not done and not open** (new, or back after the inbox resolved it): send an `upsert` with `revision` set to the later of the assignment's `revision` and `START`, so the inbox queues it again instead of calling it `stale`. Skip it when `old` is true: long-past work that has not changed lately stays out, while a recent change such as an unsubmit or an unarchive is always sent.
   - **Done** (`done` true) and open: send a `resolve` with `revision` set to the later of the assignment's `revision` and `START`.
   - **Done and not open:** send nothing.
   - **Open but missing from the output** (the course was archived or the work deleted or unpublished): send a `resolve` with `revision` set to `START`. The helper fails as a whole on any API error, so a successful output is always complete.
6. Write the batch file(s) into the run directory you were given (`<run dir>/classroom-1.json`, `-2`, ...), at most 200 events each. Put `"cursor": {"source": "classroom", "account": "<account>", "value": "<START>"}` only in the last one. With nothing to send, still post one batch with the cursor and an empty `events` list, so the inbox records the run.
7. Post each file in order: `skills/things-hub-collect/thub-send post <file>`.

## Event fields

```json
{
  "source": "classroom", "account": "university", "kind": "assignment",
  "source_id": "123456789/987654321", "op": "upsert",
  "revision": "2026-10-05T09:00:00Z", "observed_at": "2026-10-13T12:00:00Z",
  "url": "https://classroom.google.com/c/MTIz/a/OTg3/details",
  "title": "Turn in Lista 3 (Cálculo I)",
  "summary": "Cálculo I: exercises 1 to 10 of chapter 4, handwritten and scanned as one PDF. Due Oct 20, 02:59 UTC.",
  "occurred_at": "2026-10-01T10:00:00Z", "due": "2026-10-20T02:59:00Z",
  "labels": { "course": "Cálculo I", "work_type": "assignment", "state": "created" }
}
```

| Field | Rule |
| --- | --- |
| `kind` | always `assignment` |
| `source_id`, `url`, `due` | the assignment's own values; leave `due` out when it is empty |
| `revision` | as step 5 of the run says; compare times, not text |
| `observed_at` | `START` |
| `title` | at most 140 characters, imperative: `Turn in <title> (<course>)`; for a quiz or question, `Answer <title> (<course>)` |
| `summary` | at most 600 characters: what to deliver and how, from the description, plus the due date. Never the raw description |
| `occurred_at` | the assignment's `created`, when present |
| `labels` | `course`, `work_type` and `state` (lowercase), plus `late: "true"` when `late` is true. Each label value at most 200 characters |

A `resolve` carries only `source`, `account`, `kind`, `source_id`, `op`, `revision` and `observed_at`. Do not send a `key`; the server computes it. Unknown fields are rejected.

## After posting

`thub-send post` prints the server's JSON response and a count line.

| Exit | Meaning | What to do |
| --- | --- | --- |
| 0 | sent | done |
| 2 | the server refused the batch, the account (HTTP 403: the Mac's sender is not allowed `classroom/<account>`), or some events | report it; for a 403 the owner runs `thub-server sender allow mac --source classroom --account <account>` on the home server. Do not retry in this run |
| 75 | inbox unreachable or erroring | the batch stays in the spool and goes out with the next run; report it |
| 1 | settings error | report it (see docs/classroom-setup.md) |

End with a short report: courses read, assignments seen, upserts and resolves sent, the server's accepted, duplicate, stale and rejected counts, and anything that blocked the run. No titles or summaries in the report.
