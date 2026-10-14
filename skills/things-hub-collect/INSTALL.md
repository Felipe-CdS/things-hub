# Installing things-hub-collect on the company notebook

The skill needs a POSIX shell, `curl`, the notebook's Slack MCP servers, and `gh` logged in to the owner's GitHub (macOS, Linux, or WSL on Windows). Nothing else is installed.

## 1. Copy the skill

Ask the notebook's model, or run it yourself (the model already has access to the owner's GitHub):

```sh
gh repo clone Felipe-CdS/things-hub /tmp/things-hub -- --depth 1
dest=~/.claude/skills/things-hub-collect   # see below for other clients
mkdir -p "$dest"
cp /tmp/things-hub/skills/things-hub-collect/SKILL.md \
   /tmp/things-hub/skills/things-hub-collect/thub-send \
   /tmp/things-hub/skills/things-hub-collect/INSTALL.md "$dest"/
[ -f "$dest/accounts.md" ] || cp /tmp/things-hub/skills/things-hub-collect/accounts.example.md "$dest/accounts.md"
chmod +x "$dest/thub-send"
rm -rf /tmp/things-hub
```

- Claude Code: `~/.claude/skills/things-hub-collect`.
- Clients that read `~/.agents/skills` (or their own skills directory): use that path instead.
- A client without skills: keep the directory anywhere and give the model `SKILL.md` as the prompt, with its directory as the working directory.

Updating later is the same copy; it keeps your `accounts.md`.

## 2. Fill in the accounts

Edit `accounts.md`: one row per Slack workspace and GitHub org. Each Slack workspace needs its own Slack MCP server entry in the client, named in the `tool` column (such as `slack-acme`). If the client can restrict tools, allow only the Slack servers' read tools and deny the write tools (send, schedule, draft, react, mark, canvas, channel, file, list).

## 3. Get a token

On the home server, create the notebook's sender with every account from the table (a sender never gets `--reader`):

```sh
docker exec things-hub thub-server sender add notebook \
  --source slack --account T024BE7LD --source github --account acme
```

It prints the token once. Later accounts: `docker exec things-hub thub-server sender allow notebook --source slack --account T…`.

## 4. Configure thub-send

Put the settings in `~/.config/things-hub/env`, readable only by you (thub-send refuses the file otherwise):

```sh
mkdir -p ~/.config/things-hub
umask 077
cat > ~/.config/things-hub/env <<'EOF'
THUB_URL=https://things-inbox.example.com
THUB_TOKEN=thub_...
# Only when the inbox is behind Cloudflare Access:
CF_ACCESS_CLIENT_ID=...
CF_ACCESS_CLIENT_SECRET=...
EOF
chmod 600 ~/.config/things-hub/env
```

Batches wait in `~/.local/state/things-hub/spool` until the inbox takes them (`THUB_SPOOL` moves it); refused batches go to its `failed/` directory for inspection.

Check it: `"$dest/thub-send" cursor slack T024BE7LD` prints `{"source":"slack","account":"t024be7ld","value":null}` on the first run.

## 5. Run it

Ask the model to "sync work" (every account in turn) or "sync work for acme" (one account).

## Trying it locally first

Everything can run on one machine before the inbox has a public hostname. From a checkout of this repository:

```sh
export THUB_DB=tmp/thub.db THUB_LISTEN=127.0.0.1:8787
mkdir -p tmp
go run ./cmd/thub-server sender add notebook --source slack --account T024BE7LD --source github --account acme
go run ./cmd/thub-server serve &
THUB_URL=http://127.0.0.1:8787 THUB_TOKEN=thub_... skills/things-hub-collect/thub-send open github acme
```

Then point the skill's env file at `http://127.0.0.1:8787` and run it; check what arrived with a reader token (`sender add mac --reader`) and `curl -H "Authorization: Bearer $MAC_TOKEN" http://127.0.0.1:8787/v1/status`.
