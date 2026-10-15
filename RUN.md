# things-hub: the on-demand Mac run

The steps a things-hub worker follows on the owner's Mac when the owner says there is something new. The agent does the judgment (collecting GitHub with the skill, writing the plan); `thub` does everything deterministic. Re-running any step is safe: events dedupe on the server, and every Things write is ledgered and applied at most once.

## One-time setup

1. **Build thub**: `make thub` (`go install ./cmd/thub`, so `thub` lands in `$(go env GOPATH)/bin`; keep that on `PATH`). Inside a worktree, `go run ./cmd/thub <command>` works too.
2. **The inbox server** must be reachable at `THUB_URL`:
   - **Local trial on this Mac** (no SSH forward): `make build-local && make run` (Docker, `127.0.0.1:8787`), or without Docker:
     `THUB_DB="$HOME/Library/Application Support/things-hub/server/thub.db" THUB_LISTEN=127.0.0.1:8787 go run ./cmd/thub-server serve` after `mkdir -p` of that folder.
   - **Home server, phase 1**: `ssh -N -L 8787:127.0.0.1:8787 <home-server>` in another terminal.
3. **The Mac sender** (once per server database), with the server's admin command:
   - Docker: `docker exec things-hub thub-server sender add mac --source github --account felipe-cds --reader`
   - `go run`: the same `THUB_DB=… go run ./cmd/thub-server sender add mac --source github --account felipe-cds --reader`

   It prints the token once.
4. **The .env**: copy `.env.example` to `~/Library/Application Support/things-hub/.env`, fill in `THUB_TOKEN` and the rest, then `chmod 600` it. thub refuses a `.env` that group or others can read. `THUB_ENV_FILE` points thub at another file; thub never reads the repository's own `.env`, which `make run` passes to the server.
5. **Things**: Settings → General → Enable Things URLs is on (done 2026-10-13). The first run may raise macOS prompts (Automation for `osascript`, access to Things' data); attend it.

## The run

Use one run folder for every file of the run:

```sh
RUN="$HOME/Library/Application Support/things-hub/runs/$(date -u +%Y-%m-%dT%H-%M-%SZ)"
mkdir -p "$RUN"
```

### 0. Doctor

```sh
thub doctor
```

Every line must be `ok`: the `.env` (mode 600), the inbox token, the inbox reachable and readable, the `gh` login matching `THUB_GITHUB_LOGIN`, things-mcp starting with all required tools, the Things URL auth token present (its value is never printed), `osascript`. On a `FAIL`, stop and report it.

### 1. Collect GitHub (personal account)

Follow `skills/things-hub-github/SKILL.md`: it reads GitHub with the existing `gh` login, writes `$RUN/github-batch.json`, checks it with `thub check-batch` and posts it with `thub post`. Note the accepted, duplicate and stale counts for the report.

Other collectors (the notebook's company skill, other Mac routines) post to the inbox on their own; this run processes whatever is pending, whoever sent it. A later collect step, here or on the server, slots in at this point.

### 2. Pull

```sh
thub pull --out "$RUN"
```

Takes the Things snapshot (stored on the server too), settles creates a crashed run left behind, refreshes the ledger, leases the pending items (2 hours) and writes `$RUN/input.json`. If it prints `nothing new`, report that and stop.

### 3. Plan

Follow `prompts/plan.md` with `POLICY.md` and `$RUN/input.json`; write `$RUN/plan.json`. Then:

```sh
thub check "$RUN/plan.json"
```

Fix the plan until it prints `plan ok`. If the only refusal is a per-run limit, keep the plan as it is and go on to step 4 for the report.

### 4. Dry run

```sh
thub apply --dry-run "$RUN/plan.json"
```

Prints every change (also saved as `$RUN/dry-run.txt`). Put it in the run report.

**Stop here and report to Firstmate, without applying, when:**

- the plan is over a per-run limit (the dry run says `over limit:`): after the owner's one-time yes, apply with `--allow-over-limits`;
- the dry run says `layout: this plan reorganizes existing items` and the layout is not approved yet (the first such run is preview-only): after the owner's yes, apply with `--approve-layout`. thub then remembers the approval in `state.json`.

The lease lasts 2 hours. If the yes comes later, the items return to the queue; start a new run.

### 5. Apply

```sh
thub apply "$RUN/plan.json"
```

Settles pending creates, writes each op ahead to the ledger, applies it through things-mcp (or AppleScript and the URL scheme for tags, deletes and project moves), finds every new item by its marker, records the audit log on the server and in `$RUN/audit.jsonl`, acks every answered item and releases the unplanned ones. The result is in `$RUN/apply-result.json`.

If it is interrupted, run the same command again: applied ops are skipped, a half-written create is found by its marker, nothing is created twice.

### 6. Status and report

```sh
thub status "$RUN"
```

Report to Firstmate: the GitHub post counts, the dry-run diff, the created, changed, completed, deleted, ignored and deferred counts, skipped ops (changed in Things since the snapshot; they come back next run), failed ops, rejected acks and parked items.

## Undo

```sh
thub undo <run-id>
```

Writes back the inverse of a run's changes, newest first: updates and moves get their before state, completions and cancels are reopened, creates are canceled, deletes come back out of the Trash (or are recreated from the snapshot if that fails, and the copy takes over the original's source links), a deleted area is recreated under a new uuid and the to-dos moved out of it go back into it, project moves go back. It is recorded as run `undo-<run-id>`; running it again only does what is left. A created area is deleted only if it is empty, and a created tag only if no item uses it.

## Files

| Where | What |
| --- | --- |
| `~/Library/Application Support/things-hub/.env` | settings and secrets (mode 600; keys in `.env.example`) |
| `~/Library/Application Support/things-hub/state.json` | the layout approval |
| `~/Library/Application Support/things-hub/runs/<run-id>/` | `github-batch.json`, `input.json`, `plan.json`, `dry-run.txt`, `audit.jsonl`, `apply-result.json` |
| `~/Library/Application Support/things-hub/spool/` | event batches waiting for `thub flush` |
