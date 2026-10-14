# things-hub

Gathers what the owner has to do from Slack, GitHub, Google Classroom, Gemini meeting notes and other sources, and organizes it in Things 3.

Design: see the owner's wiki (`~/Docs/wiki/things-hub/DESIGN.md`).

## Layout

- `cmd/thub-server/`: the inbox server for the home server (Go, no LLM).
- `api/`: wire types shared by the server and its clients: the event envelope and the pull, ack, ledger, audit, snapshot and status shapes.
- `internal/store/`: SQLite storage, queue and lease logic. `internal/server/`: the HTTP API.
- `skills/things-hub-collect/`: the company notebook's collector skill (`SKILL.md`) and `thub-send`, its POSIX sh and curl client for the inbox. Install steps are in its `INSTALL.md`.

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
| `make test` | `go vet ./...`, `go test ./...`, and the `thub-send` test against a locally started server |

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
