# things-hub accounts

The accounts `things-hub-collect` may read, one run per row. Copy this file to `accounts.md` next to `SKILL.md` and replace the example rows. Each row must also be allowed on the notebook's sender on the server (see INSTALL.md), or the server refuses its batches (thub-send exits 2 and moves them to the spool's `failed/` directory).

| alias | source | account | tool | owner | signals |
| --- | --- | --- | --- | --- | --- |
| acme | slack | T024BE7LD | slack-acme | U012ABCDEF | mention, dm-awaiting-reply, thread-question |
| acme-gh | github | acme | gh | @me | review-requested, assigned, pr-needs-fix |

- `account`: the Slack team id (`T…`) or the GitHub org name. It becomes the event's `account` exactly as written here.
- `tool`: the Slack MCP server configured for that workspace only, or `gh` (or the GitHub MCP's name) for an org.
- `owner`: the owner's Slack user id in that workspace (`U…`), or `@me` for GitHub.
- `signals`: the kinds to collect for that account; leave out any the tool cannot read.
- Hexweavers is not listed here: the Mac collects it.
