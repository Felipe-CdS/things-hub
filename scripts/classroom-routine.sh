#!/bin/sh
# classroom-routine.sh: one run of the Classroom routine on the Mac. launchd
# starts it (see launchd/things-hub-classroom.plist.in); `make classroom-run`
# runs it by hand. It starts headless Claude Code on the owner's own Claude
# login (never an API key) with the things-hub-classroom skill, which reads
# Classroom through bin/thub-classroom and posts events with thub-send.
#
# Settings live in the repository's .env (mode 600), shared by
# thub-classroom and thub-send. CLASSROOM_RUN_TIMEOUT (seconds, default 1200)
# bounds one run.

set -u

repo=$(cd "$(dirname "$0")/.." && pwd -P)
cd "$repo" || exit 1

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) classroom-routine: $*"; }

# The run must use the Claude login, never an API key from the environment.
unset ANTHROPIC_API_KEY

export THUB_ENV_FILE="$repo/.env"
helper=bin/thub-classroom
send=skills/things-hub-collect/thub-send

for f in .env "$helper" "$send" skills/things-hub-classroom/SKILL.md; do
	if [ ! -e "$f" ]; then
		log "missing $repo/$f; see docs/classroom-setup.md"
		exit 1
	fi
done
if ! command -v claude >/dev/null 2>&1; then
	log "claude is not on PATH ($PATH)"
	exit 1
fi

run=$(mktemp -d "${TMPDIR:-/tmp}/things-hub-classroom.XXXXXX") || exit 1
run=$(cd "$run" && pwd -P)
trap 'rm -rf "$run"' EXIT

prompt="Run the things-hub Classroom collection once. Follow skills/things-hub-classroom/SKILL.md exactly. \
The run directory for batch files is $run. You run unattended: do not ask questions; end with the report the skill describes."

log "start (run directory $run)"
claude -p "$prompt" \
	--permission-mode dontAsk \
	--strict-mcp-config \
	--add-dir "$run" \
	--allowedTools \
	"Read" \
	"Bash(date -u *)" \
	"Bash($helper assignments)" \
	"Bash($send flush)" \
	"Bash($send open classroom *)" \
	"Bash($send post $run/*)" \
	"Write(/$run/**)" &
pid=$!
(
	sleep "${CLASSROOM_RUN_TIMEOUT:-1200}"
	log "run timed out; stopping it"
	kill "$pid" 2>/dev/null
) &
watchdog=$!
wait "$pid"
rc=$?
kill "$watchdog" 2>/dev/null
log "end (exit $rc)"
exit "$rc"
