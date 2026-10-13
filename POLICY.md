# things-hub policy

What the agent may do in Things 3 during an on-demand run, and the guard rails `thub` enforces. The agent reads this file with `prompts/plan.md` before writing `plan.json`. The owner changes it by an explicit decision.

## Ownership

The owner decided (2026-10-13) that the LLM owns Things: it adds, updates, reschedules, moves, completes, cancels and deletes to-dos, projects, areas and tags, including items that existed before things-hub. The owner does not add or update anything by hand. The current layout is not binding: new areas, projects, headings and tags may be created as needed.

There is no "owner edits win" rule and no "add only" rule. The guard rails below protect against accidents (a runaway plan, a crash), not against the agent's judgment.

## What counts as a task

- A source item becomes a to-do when someone asks the owner to do, answer, review, fix or decide something, or a source states a due date for them.
- Everything else is `ignore`, always with a reason: FYIs, bots, items the owner already handled, duplicates of an existing to-do (prefer `attach` to the existing one).
- `defer` an item that needs context the run does not have yet, with a reason (and `not_before` when there is a natural time).

## Shape

- **Decomposition:** a to-do with a checklist for up to about 5 steps; above that, a project with one to-do per step (`action: project`). `thub check` warns when a plan departs from this.
- **Placement:** put each item in the area or project where the owner would look for it. Create areas, projects and tags when the existing layout has no good home.
- **Tags:** tag GitHub items `github`. Create a tag with a `create_tag` op before using it.
- **Headings:** a step may go under an existing heading. thub cannot create a heading in an existing project.

## Safety (enforced by thub)

- **Never duplicate.** One live Things item per source key: `thub check` refuses a second create for a key that already has a live (open or pending) link; use `attach`, `update` or `resolve` instead. Every hub-made item carries a marker in its notes, so a crashed or repeated run never creates it twice.
- **Per-run limits.** `thub check` refuses a plan above them:

  | Limit | Default | `.env` key |
  | --- | --- | --- |
  | New to-dos (including a new project's steps) | 15 | `THUB_LIMIT_NEW_TODOS` |
  | Deletes (to-dos and projects to the Trash) | 10 | `THUB_LIMIT_DELETES` |
  | Moves, reschedules and other changes to items that existed before the run | 50 | `THUB_LIMIT_MOVES` |
  | Area creations and deletions | 3 | `THUB_LIMIT_AREA_CHANGES` |

  A plan above a limit is not split or trimmed silently: the worker brings the dry run to Firstmate for a one-time yes, then applies with `--allow-over-limits`.
- **The first layout-reorganizing run is preview-only.** The first plan whose organize ops change existing items (moves, updates, completions, deletes, area renames or deletes) stops at the dry run. Firstmate shows it to the owner; after their yes the worker applies it with `--approve-layout`, which `thub` records in `state.json`. Later runs apply directly when within the limits.
- **Deletes only go to the Trash.** thub never empties the Trash; no plan op exists for it. An area is deleted only when it is empty: the plan must move or delete its contents first. For something done or obsolete, prefer `complete` or `cancel`, which keep it in the Logbook.
- **No clobbering mid-run.** Right before each change to an existing item, thub re-reads it; if it changed since the snapshot, the op is skipped and its source item comes back next run.
- **Audit and undo.** Every applied change is recorded with its before and after state, on the server and in the run folder. `thub undo <run-id>` writes back the inverse of a run, newest first.

## Completion rules

- **GitHub auto-complete.** When the collector reports a GitHub item resolved (PR merged or closed, review submitted, issue closed or unassigned) and its to-do is still open, the plan completes it with `action: resolve`. `thub check` refuses any other answer for such an item, except `defer`.
- **Things is the truth.** A to-do the owner completed stays completed; a new obligation from the same source becomes a new to-do only when the source raises something new (a new revision with a new ask).
- The completed "things-hub connection test" to-do in the Logbook is a leftover of the 2026-10-13 setup check; ignore it.

## Retention

- Event summaries on the server: 90 days (`THUB_RETENTION_DAYS`). Keys and revisions stay, so dedupe keeps working.
- Things snapshots: the last 30 runs on the server; run folders on the Mac stay as an audit trail.

## Organizing-only runs

None by default. When the queue is empty, `thub pull` stops with "nothing new" and the worker reports that.
