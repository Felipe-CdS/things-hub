# Write plan.json for a things-hub run

You are step 3 of a things-hub run (RUN.md). `thub pull` wrote `$RUN/input.json`; you write `$RUN/plan.json`, and `thub` carries it out. You decide; you never write to Things yourself.

Read first: `POLICY.md` (what counts as a task, decomposition, limits, completion rules), then `$RUN/input.json`.

## What input.json holds

- `run_id`, `lease_id`: copy both into the plan unchanged.
- `items`: the leased source items. Each has `key`, `op` (`upsert`: exists or changed; `resolve`: no longer needs the owner), `revision`, `event` (title, summary, url, from, due, labels), `previous` (the last outcome, if any) and `things` (the Things items already linked to this key, with `state`: `pending_create`, `open`, `completed`, `canceled`, `trashed`, `gone`).
- `snapshot`: all of Things at the start of the run: `areas`, `projects`, `headings`, `tags`, open `todos`, and to-dos `completed`, `canceled` or `trashed` in the last 30 days. Each item has its `uuid`, `title`, `notes`, `area`, `project`, `heading`, `tags`, `start`, `start_date`, `deadline`, `checklist`, `modified`.
- `ledger`: every Things item the hub made (with its marker) or adopted.
- `limits`, `layout_approved`.

You may also read Things through things-mcp's read tools if they are available in the session. Never call its write tools, `show_item` or `search_items`.

## Answer every item

Give each leased `key` exactly one decision in `items`. A key you leave out is released back to the queue unprocessed.

| `action` | When | Fields |
| --- | --- | --- |
| `create` | a new obligation, no live linked item | `todo` |
| `project` | a new obligation with more than about 5 steps | `project`, `todos` (the steps) |
| `attach` | the same request as an open to-do (a duplicate from another source, or a newer revision of a linked one) | `uuid` of the to-do; optional `append_checklist`, `fields` |
| `update` | a newer revision changes a linked item (title, due date, list) | `uuid`, `fields` |
| `resolve` | `op` is `resolve`, or the obligation is over | optional `uuid` (default: the live linked items); `cancel: true` to cancel instead of complete |
| `ignore` | not an obligation | `reason` (required) |
| `defer` | needs context the run does not have | `reason` (required), optional `not_before` (RFC 3339) |

Rules `thub check` enforces: no `create` or `project` for a key whose `things` has a live (`open` or `pending_create`) item; a GitHub item with `op: resolve` and an open linked to-do must be `resolve` (or `defer`); `revision`, when you give it, must equal the leased one.

## Organize (optional)

`organize` holds ops on Things itself, with no key. They run **before** `items`, in order, so items may use areas, projects and tags created here.

| `op` | Fields |
| --- | --- |
| `create_area` | `title` |
| `rename_area` | `uuid`, `title` |
| `delete_area` | `uuid` of an empty area (move or delete its contents first) |
| `create_tag` | `title` |
| `create_project` | `title`, optional `area`, `summary` |
| `move` | `uuid` of a to-do, `list` |
| `move_project` | `uuid`, `area` |
| `update` | `uuid` of a to-do or project, `fields` |
| `complete`, `cancel` | `uuid` |
| `delete` | `uuid` of a to-do or project, `reason` (goes to the Trash) |

There is no way to empty the Trash.

## Field formats

- `todo`: `title` (imperative, at most 255 characters), `summary` (what and who, short; thub builds the notes with the source line, link and marker, so never write notes for a new item), `list` (area or project), `heading`, `when`, `deadline`, `tags`, `checklist` (titles).
- `project`: `title`, `summary`, `area`, `when`, `deadline`, `tags`. Its `todos` take `title`, `summary`, `when`, `deadline`, `tags`, `checklist`; they go into the new project.
- `fields`: any of `title`, `notes`, `when`, `deadline` (`""` clears it), `tags` (replaces them), `list`, `heading`, `checklist` (replaces it). Keep an item's marker line (`th-…`) if you rewrite its notes.
- `list`, `area`: a uuid, or the exact title of an area or project in the snapshot or created earlier in the plan. Use the uuid when a title is ambiguous.
- `when`: `today`, `tomorrow`, `evening`, `anytime`, `someday`, `YYYY-MM-DD`, or one of those with `@HH:MM` for a reminder. `deadline`: `YYYY-MM-DD`; use the event's `due` when it has one.
- `tags`: existing tag titles, or ones created by `create_tag` in this plan.

## Example

```json
{
  "schema": "things-hub.plan.v1",
  "run_id": "2026-10-13T14-02-11Z",
  "lease_id": "l_7q2…",
  "organize": [
    { "op": "create_tag", "title": "github" }
  ],
  "items": [
    { "key": "github:felipe-cds:review-requested:Felipe-CdS/mic#12", "action": "create",
      "todo": { "title": "Review PR: microcode loader (mic#12)", "summary": "Ana requested your review.",
                "list": "Work", "when": "today", "tags": ["github"] } },
    { "key": "github:felipe-cds:assigned:Felipe-CdS/site#3", "action": "resolve" },
    { "key": "github:felipe-cds:pr-needs-fix:Felipe-CdS/mic#9", "action": "attach",
      "uuid": "<open to-do uuid>", "append_checklist": ["Fix failing lint check"] }
  ]
}
```

## Then

Run `thub check $RUN/plan.json` and fix the plan until it prints `plan ok`. Over a limit, do not trim the plan to fit: keep it, and say so in the run report so Firstmate can ask the owner. Then continue with RUN.md step 4.
