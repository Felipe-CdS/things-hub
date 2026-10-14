#!/bin/sh
# Tests scripts/classroom-routine.sh with a fake claude on PATH: it must run
# from the repository, drop ANTHROPIC_API_KEY, allow only the helper and
# thub-send commands, scope Read and file writes, and stop when a piece is missing.

set -u

here=$(cd "$(dirname "$0")/../.." && pwd -P)
tmp=$(mktemp -d) || exit 1
tmp=$(cd "$tmp" && pwd -P)
trap 'rm -rf "$tmp"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

repo=$tmp/repo
mkdir -p "$repo/scripts" "$repo/bin" "$repo/skills/things-hub-collect" "$repo/skills/things-hub-classroom" "$tmp/path"
cp "$here/scripts/classroom-routine.sh" "$repo/scripts/"
: >"$repo/bin/thub-classroom"
: >"$repo/skills/things-hub-collect/thub-send"
: >"$repo/skills/things-hub-classroom/SKILL.md"

cat >"$tmp/path/claude" <<EOF
#!/bin/sh
{
	echo "pwd=\$(pwd -P)"
	echo "key=\${ANTHROPIC_API_KEY:-unset}"
	echo "env_file=\$THUB_ENV_FILE"
	for a in "\$@"; do echo "arg=\$a"; done
} >"$tmp/claude.out"
exit \${FAKE_CLAUDE_EXIT:-0}
EOF
chmod +x "$tmp/path/claude"

# Missing .env: refuses before starting claude.
out=$(PATH="$tmp/path:/usr/bin:/bin" sh "$repo/scripts/classroom-routine.sh" 2>&1) && fail "ran without .env"
echo "$out" | grep -q "missing $repo/.env" || fail "no missing .env message: $out"
[ ! -e "$tmp/claude.out" ] || fail "claude started without .env"

: >"$repo/.env"
chmod 600 "$repo/.env"
ANTHROPIC_API_KEY=sk-test PATH="$tmp/path:/usr/bin:/bin" FAKE_CLAUDE_EXIT=3 \
	sh "$repo/scripts/classroom-routine.sh" >"$tmp/log" 2>&1
rc=$?
[ "$rc" = 3 ] || fail "exit $rc, want claude's 3: $(cat "$tmp/log")"
grep -q "end (exit 3)" "$tmp/log" || fail "log: $(cat "$tmp/log")"

c=$tmp/claude.out
grep -qx "pwd=$repo" "$c" || fail "not run from the repository: $(cat "$c")"
grep -qx "key=unset" "$c" || fail "ANTHROPIC_API_KEY reached claude"
grep -qx "env_file=$repo/.env" "$c" || fail "THUB_ENV_FILE not set"
grep -qx "arg=-p" "$c" || fail "not headless"
grep -qx "arg=dontAsk" "$c" || fail "permission mode not dontAsk"
grep -qx "arg=Bash(bin/thub-classroom assignments)" "$c" || fail "helper not allowed"
grep -qx "arg=Bash(skills/things-hub-collect/thub-send open classroom \*)" "$c" || fail "open not allowed"
if grep '^arg=Bash(' "$c" | grep -v -e 'thub-classroom assignments)' -e 'thub-send ' -e 'date -u'; then
	fail "unexpected Bash permission"
fi
run=$(awk 'p { sub(/^arg=/, ""); print; exit } $0 == "arg=--add-dir" { p = 1 }' "$c")
[ -n "$run" ] || fail "no run directory"
grep -qxF "arg=Bash(skills/things-hub-collect/thub-send post $run/*)" "$c" || fail "post not scoped to the run directory"
grep -qxF "arg=Edit(/$run/**)" "$c" || fail "file writes not scoped to the run directory"
if grep '^arg=Read' "$c" | grep -vxF -e "arg=Read(skills/things-hub-classroom/**)" -e "arg=Read(/$run/**)"; then
	fail "Read not scoped to the skill and run directories"
fi
grep -qxF "arg=Read(skills/things-hub-classroom/**)" "$c" || fail "skill not readable"
grep -qxF "arg=Read(/$run/**)" "$c" || fail "run directory not readable"
[ ! -e "$run" ] || fail "run directory $run left behind"

echo "classroom-routine test: ok"
