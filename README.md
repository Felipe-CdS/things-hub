# things-hub

Gathers what the owner has to do from Slack, GitHub, Google Classroom, Gemini meeting notes and other sources, and organizes it in Things 3.

Design: see the owner's wiki (`~/Repositories/compendium/wiki/things-hub/DESIGN.md`).

## Layout

- `cmd/thub-server/`: the inbox server for the home server (Go, no LLM).
- `api/`: wire types shared by the server and its clients: the event envelope and the pull, ack, ledger, audit, snapshot and status shapes.
- `internal/store/`: SQLite storage, queue and lease logic. `internal/server/`: the HTTP API.
- `cmd/thub-classroom/`, `internal/classroom/`: the Mac-side Classroom helper: the one-time Google login and read-only Classroom reads, printed as JSON. It can be folded into the Mac tool `thub` later.
- `skills/things-hub-classroom/`: the Classroom skill. `scripts/classroom-routine.sh` and `launchd/` run it on a schedule with headless Claude Code.
- `cmd/thub/`: the Mac tool (Go): pull, check, apply, undo, status and the event helpers. `internal/things/`: Things 3 through things-mcp, a read-only view of the Things database, and AppleScript or URL-scheme fallbacks; `internal/things/thingstest/`: an in-memory Things for tests. `internal/plan/`: the run files and plan validation. `internal/runner/`: pull, apply and undo. `internal/hub/`: the HTTP client. `internal/events/`: batch checks and the spool. `internal/macconf/`: the `.env` and local state.
- `RUN.md`: the on-demand Mac run. `POLICY.md`: what the agent may do in Things, and the limits. `prompts/plan.md`: how the agent writes the plan. `skills/things-hub-github/`: the personal GitHub collector skill.
- `docs/`: setup guides.

## Who produces events

The server only receives, stores and hands out events; it never collects anything and holds no source credentials. Events come from:

- the company notebook (Slack workspaces, org GitHub), through its collector skill;
- the Mac: a scheduled Claude routine for Google Classroom, and the on-demand run's own sources.

Organizing the events into Things stays on demand.

## Classroom routine (Mac)

A launchd job runs headless Claude Code every hour while the Mac is awake, on the owner's Claude login (never an API key), with the `things-hub-classroom` skill. The skill reads Classroom through `bin/thub-classroom` (read-only scopes) and posts `assignment` events with `thub-send`; a turned-in or returned submission resolves its item. Secrets live in the gitignored `.env` (mode 600; see `.env.example`).

| Target | What it does |
| --- | --- |
| `make classroom-login` | Builds the helper and runs the one-time Google login in the browser; the refresh token goes into `.env` |
| `make classroom-run` | One routine run now, in the foreground |
| `make classroom-install` | Installs the launchd job (`CLASSROOM_INTERVAL`, seconds, default 3600) |
| `make classroom-uninstall` | Removes it |

Setup, from the Google Cloud project to the first run: [`docs/classroom-setup.md`](docs/classroom-setup.md).

## thub-server

One static Go binary with pure-Go SQLite (WAL mode), shipped as one Docker image. It stores collector events, deduplicates them by a key it computes (`source:account:kind:source_id`), queues them for the Mac with leases, and keeps the Things ledger, the audit log and the last 30 Things snapshots. Logs carry keys and counts, never titles or summaries.

### Build and run

The Makefile follows the same pattern as `Felipe-CdS/basement`; images are tagged with the short git sha.

| Target | What it does |
| --- | --- |
| `make build` | Multi-arch (`linux/amd64`, `linux/arm64`) `docker buildx build --push` of `coutito/things-hub:<sha>` |
| `make build-local` | Builds the image for this machine only and loads it locally, without pushing |
| `make run` | `docker run -d` on `127.0.0.1:8787`, database in the `thub-data` volume, `.env` as environment if present |
| `make dev` | `docker compose --profile hot up -d`: rebuilds and restarts on every Go change (air), database in `tmp/` |
| `make test` | `go vet ./...`, `go test ./...`, and the Classroom routine script test |
| `make thub` | Installs the Mac tool, `thub`, with `go install ./cmd/thub` |

The image is distroless, runs as non-root, and `make run` adds a read-only root filesystem. Settings come from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `THUB_DB` | `/data/thub.db` | Database file |
| `THUB_LISTEN` | `:8787` | Listen address |
| `THUB_RETENTION_DAYS` | `90` | Event payloads older than this are pruned (daily, and by `prune`); keys and revisions stay, so dedupe keeps working |

The port is published only on `127.0.0.1`. In phase 1 the Mac reaches it over an SSH forward: `ssh -N -L 8787:127.0.0.1:8787 <home-server>`.

### Administration

Administration runs inside the container, not over HTTP:

```sh
docker exec things-hub thub-server sender add mac --source github --account felipe-cds --reader
docker exec things-hub thub-server sender add notebook --source slack --account T024BE7LD
docker exec things-hub thub-server sender allow notebook --source github --account some-org
docker exec things-hub thub-server sender list
docker exec things-hub thub-server sender revoke notebook
docker exec things-hub thub-server backup /data/backup-2026-10-13.db
docker exec things-hub thub-server prune --days 90
```

- `sender add` prints the new token once; the server keeps only its SHA-256. `--source` and `--account` pair up and can repeat.
- A sender may write events, read its cursor and list open items only for its source and account pairs. `--reader` also lets it pull, ack, release, and use the ledger, audit, snapshots and status; only the Mac gets it.
- `backup` writes a consistent copy with `VACUUM INTO`; the path must not exist yet. Copy it off `/data` afterwards.

### API

Every route except `/healthz` needs `Authorization: Bearer thub_…`. Shapes are in `api/api.go`.

| Route | Scope | Purpose |
| --- | --- | --- |
| `POST /v1/events` | writer | Batch of up to 200 events, 1 MB; per-event `accepted`, `duplicate`, `stale` or `rejected` with a reason. The cursor is stored only when nothing was rejected |
| `GET /v1/cursor?source=&account=` | writer | Last stored cursor |
| `GET /v1/open?source=&account=` | writer | Items whose latest event is an upsert, so a collector can send `resolve` |
| `POST /v1/pull` | reader | Lease up to `max` ready items, oldest first (default 50 items, 2 hours) |
| `POST /v1/ack` · `POST /v1/release` | reader | Per-item outcome for a lease, idempotent · give unacked items back |
| `GET /v1/ledger` · `PUT /v1/ledger/items` | reader | Read and upsert ledger rows (by marker, then uuid), links and applied ops |
| `POST /v1/audit` · `GET /v1/audit?run=` | reader | Append (unique by run and op id) and read the change log |
| `PUT /v1/snapshots/{run}` · `GET /v1/snapshots/{run}` | reader | Store and fetch a run's Things snapshot |
| `GET /v1/status` | reader | Queue counts, oldest pending item, last event per sender and account, parked items, live leases |
| `GET /healthz` | none | Liveness |

Queue rules: a newer revision puts a done or parked item back to pending; an ack at an older revision than the stored one leaves the item pending; an expired lease's items can be pulled again, and the old lease may still ack an item no other lease took; three `failed` acks park an item until a newer revision arrives.

## thub (the Mac tool)

The Mac half of a run: the agent decides, `thub` applies. The steps are in `RUN.md`; the rules in `POLICY.md`.

| Command | What it does |
| --- | --- |
| `thub doctor` | Checks the `.env`, the inbox, the `gh` login, things-mcp and the Things URL token (never printed) |
| `thub pull --out <run>` | Takes the Things snapshot, settles crashed creates, refreshes the ledger, leases pending items, writes `input.json` |
| `thub check <plan.json>` | Validates the plan: known uuids, areas, projects and tags; no second create for a linked key; only empty areas deleted; per-run limits |
| `thub apply [--dry-run] <plan.json>` | Prints the diff, or applies: write-ahead ledger rows with markers, things-mcp writes, audit, ack and release |
| `thub undo <run-id>` | Writes back the inverse of a run's changes, newest first |
| `thub status [run]` | Queue counts, parked items, leases and a run's result |
| `thub check-batch`, `post`, `flush`, `cursor`, `open` | Helpers for collector skills: validate and send event batches, read cursors and open items |

Things writes go through things-mcp (`uvx things-mcp`, driven over stdio with the official Go MCP SDK); verified tool names are in `internal/things/mcp.go`. What things-mcp cannot do (create a tag, delete to the Trash, move a project to another area, bring an item back from the Trash) goes through AppleScript with an argv list, or the URL scheme with the token read from the Things database. Settings and secrets live in a mode-600 `.env` (keys in `.env.example`); there is no Keychain.

Two changes from the design (owner, 2026-10-13): secrets are in the `.env`, not the macOS Keychain; and GitHub is collected by a skill the run's agent follows (`skills/things-hub-github/`), posting through `thub post`, instead of a Go `thub collect github`.
