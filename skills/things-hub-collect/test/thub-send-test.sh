#!/bin/sh
# End-to-end test of thub-send against a locally started thub-server: an
# accepted batch, a duplicate resend, cursor and open, spool on failure and
# flush, refused batches, and flush order. Run from anywhere; needs go and curl.

set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
send=$here/../thub-send
work=$(mktemp -d)
server_pid=

cleanup() {
	[ -z "$server_pid" ] || kill "$server_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

fails=0
pass() { echo "ok   $1"; }
fail() {
	echo "FAIL $1" >&2
	fails=$((fails + 1))
}
# expect <name> <want exit> <got exit>
expect_rc() {
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: exit $3, want $2"; fi
}
# expect_grep <name> <pattern> <file>
expect_grep() {
	if grep -q -- "$2" "$3"; then pass "$1"; else
		fail "$1: no '$2' in $3:"
		cat "$3" >&2
	fi
}
spooled() { ls "$THUB_SPOOL"/*.json 2>/dev/null | wc -l | tr -d ' '; }
failed() { ls "$THUB_SPOOL"/failed/*.json 2>/dev/null | wc -l | tr -d ' '; }

(cd "$root" && go build -o "$work/thub-server" ./cmd/thub-server)
export THUB_DB="$work/thub.db"

port=$((20000 + $$ % 20000))
export THUB_LISTEN="127.0.0.1:$port"
"$work/thub-server" serve >"$work/server.log" 2>&1 &
server_pid=$!
i=0
until curl -fsS "http://$THUB_LISTEN/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 100 ] || {
		cat "$work/server.log" >&2
		echo "thub-server did not start" >&2
		exit 1
	}
	sleep 0.1
done

token=$("$work/thub-server" sender add notebook --source slack --account T0TEST --source github --account acme | tail -n 1)
case $token in thub_*) ;; *)
	echo "unexpected sender add output: $token" >&2
	exit 1
	;;
esac

export THUB_URL="http://$THUB_LISTEN" THUB_TOKEN="$token"
export THUB_SPOOL="$work/spool" THUB_ENV_FILE="$work/no-env-file"

batch() { # batch <file> <cursor value> <revision>
	cat >"$1" <<EOF
{"schema":"things-hub.batch.v1",
 "cursor":{"source":"slack","account":"T0TEST","value":"$2"},
 "events":[{"source":"slack","account":"T0TEST","kind":"mention","source_id":"C01/1760356800.000100",
  "op":"upsert","revision":"$3","observed_at":"2026-10-13T12:00:00Z",
  "url":"https://acme.slack.com/archives/C01/p1760356800000100","title":"Answer Ana about the deploy",
  "labels":{"channel":"#deploys"}}]}
EOF
}

# Accepted.
batch "$work/b1.json" "2026-10-13T12:00:00Z" "1760356800.000100"
rc=0
"$send" post "$work/b1.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "post accepted exits 0" 0 "$rc"
expect_grep "post reports accepted" '"result":"accepted"' "$work/out"
expect_grep "post stores the key" '"key":"slack:t0test:mention:C01/1760356800.000100"' "$work/out"
expect_grep "post stores the cursor" '"cursor_stored":true' "$work/out"
expect_grep "post prints counts" '1 accepted' "$work/err"
[ "$(spooled)" = 0 ] && pass "spool is empty after a send" || fail "spool not empty after a send"
[ -f "$work/b1.json" ] && pass "post leaves the caller's file" || fail "post removed the caller's file"

# Duplicate.
rc=0
"$send" post "$work/b1.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "resend exits 0" 0 "$rc"
expect_grep "resend reports duplicate" '"result":"duplicate"' "$work/out"

# Cursor and open.
"$send" cursor slack T0TEST >"$work/out"
expect_grep "cursor returns the stored value" '"value":"2026-10-13T12:00:00Z"' "$work/out"
"$send" open slack T0TEST >"$work/out"
expect_grep "open lists the item" '"source_id":"C01/1760356800.000100"' "$work/out"
rc=0
"$send" cursor slack TOTHER >"$work/out" 2>"$work/err" || rc=$?
expect_rc "cursor outside the sender's scope exits 1" 1 "$rc"
expect_grep "cursor outside the scope reports 403" 'HTTP 403' "$work/err"

# Spool on failure: the inbox is unreachable.
batch "$work/b2.json" "2026-10-13T13:00:00Z" "1760360400.000100"
rc=0
THUB_URL="http://127.0.0.1:1" "$send" post "$work/b2.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "post to an unreachable inbox exits 75" 75 "$rc"
[ "$(spooled)" = 1 ] && pass "the batch stays in the spool" || fail "spool holds $(spooled) batches, want 1"

# Spool on failure: a bad token keeps the batch too.
batch "$work/b3.json" "2026-10-13T14:00:00Z" "1760364000.000100"
sleep 1 # distinct spool timestamps, so flush order is defined
rc=0
THUB_TOKEN="thub_wrong" "$send" post "$work/b3.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "post with a bad token exits 75" 75 "$rc"
expect_grep "bad token reports 401" 'HTTP 401' "$work/err"
[ "$(spooled)" = 2 ] && pass "both batches are spooled" || fail "spool holds $(spooled) batches, want 2"

# Flush sends both, oldest first, so the newest cursor wins.
rc=0
"$send" flush >"$work/out" 2>"$work/err" || rc=$?
expect_rc "flush exits 0" 0 "$rc"
[ "$(spooled)" = 0 ] && pass "flush empties the spool" || fail "spool holds $(spooled) batches after flush"
expect_grep "flush reports what is left" '0 batch(es) left' "$work/err"
"$send" cursor slack T0TEST >"$work/out"
expect_grep "flush keeps the newest cursor" '"value":"2026-10-13T14:00:00Z"' "$work/out"
rc=0
"$send" flush >"$work/out" 2>"$work/err" || rc=$?
expect_rc "flush with an empty spool exits 0" 0 "$rc"

# A rejected event moves the batch to failed and keeps the cursor.
cat >"$work/b4.json" <<'EOF'
{"schema":"things-hub.batch.v1",
 "cursor":{"source":"slack","account":"T0TEST","value":"2026-10-13T15:00:00Z"},
 "events":[{"source":"slack","account":"T0TEST","kind":"mention","source_id":"C01/1760368000.000100",
  "op":"upsert","revision":"1760368000.000100","observed_at":"2026-10-13T15:00:00Z"}]}
EOF
rc=0
"$send" post "$work/b4.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "post with a rejected event exits 2" 2 "$rc"
expect_grep "the response names the reason" 'url is required for upsert' "$work/out"
[ "$(failed)" = 1 ] && pass "the rejected batch moves to failed" || fail "failed holds $(failed) batches, want 1"
"$send" cursor slack T0TEST >"$work/out"
expect_grep "a rejected batch does not move the cursor" '"value":"2026-10-13T14:00:00Z"' "$work/out"

# Invalid JSON is refused with 400 and moved to failed.
echo '{"schema":"things-hub.batch.v1","events":[],"extra":1}' >"$work/b5.json"
rc=0
"$send" post "$work/b5.json" >"$work/out" 2>"$work/err" || rc=$?
expect_rc "post of an invalid batch exits 2" 2 "$rc"
expect_grep "invalid batch reports 400" 'HTTP 400' "$work/err"
[ "$(failed)" = 2 ] && pass "the invalid batch moves to failed" || fail "failed holds $(failed) batches, want 2"

# Resolve closes the item.
cat >"$work/b6.json" <<'EOF'
{"schema":"things-hub.batch.v1",
 "events":[{"source":"slack","account":"T0TEST","kind":"mention","source_id":"C01/1760356800.000100",
  "op":"resolve","revision":"2026-10-13T16:00:00Z","observed_at":"2026-10-13T16:00:00Z"}]}
EOF
"$send" post "$work/b6.json" >"$work/out" 2>"$work/err"
expect_grep "resolve is accepted" '"result":"accepted"' "$work/out"
"$send" open slack T0TEST >"$work/out"
expect_grep "open is empty after the resolve" '"items":\[\]' "$work/out"

# Settings load from a private env file; a loose one is refused. The token
# is never printed.
printf 'THUB_URL=%s\nTHUB_TOKEN=%s\n' "$THUB_URL" "$THUB_TOKEN" >"$work/env"
chmod 644 "$work/env"
rc=0
env -u THUB_URL -u THUB_TOKEN THUB_ENV_FILE="$work/env" "$send" open github acme >"$work/out" 2>"$work/err" || rc=$?
expect_rc "a group-readable env file is refused" 1 "$rc"
chmod 600 "$work/env"
env -u THUB_URL -u THUB_TOKEN THUB_ENV_FILE="$work/env" "$send" open github acme >"$work/out"
expect_grep "settings load from the env file" '"account":"acme"' "$work/out"
THUB_TOKEN="thub_wrong" "$send" open github acme >"$work/out" 2>"$work/err" || true
if grep -q thub_wrong "$work/out" "$work/err"; then fail "thub-send printed the token"; else pass "thub-send never prints the token"; fi

if [ "$fails" -ne 0 ]; then
	echo "$fails check(s) failed" >&2
	exit 1
fi
echo "all thub-send checks passed"
