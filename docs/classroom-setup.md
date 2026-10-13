# Google Classroom setup

The Classroom source runs on the Mac. A launchd job starts headless Claude Code every hour while the Mac is awake. Claude Code uses your own Claude login, never an API key, and runs the `things-hub-classroom` skill (`skills/things-hub-classroom/SKILL.md`). The skill reads Classroom through `bin/thub-classroom`, a small read-only helper, and posts the events to the inbox with `thub-send`. The home server only receives the events. Organizing them into Things stays on demand.

You do steps 1 to 6 once. Everything uses read-only access: things-hub never writes to Classroom.

## What you need

- This repository checked out on the Mac, with Go installed (to build the helper).
- Claude Code logged in on the Mac with your Claude Team account (`claude` works in a terminal). Do not set `ANTHROPIC_API_KEY`; the routine removes it from its environment so the run always uses your login.
- `skills/things-hub-collect/thub-send`, the inbox client, which arrives with the company skill (PR https://github.com/Felipe-CdS/things-hub/pull/2).
- The inbox reachable from the Mac at `THUB_URL`. In phase 1 that is the SSH forward `ssh -N -L 8787:127.0.0.1:8787 <home-server>`. While the inbox is unreachable, batches wait in thub-send's spool and go out with the next run.

## 1. Create the Google Cloud project

Any Google account can own the project, such as your personal one. It does not have to be the account that holds Classroom.

1. Open https://console.cloud.google.com/ and create a project, for example `things-hub`.
2. **APIs & Services → Library**: search for **Google Classroom API** and click **Enable**.

## 2. Set up the consent screen (External, In production)

In **Google Auth Platform** (APIs & Services → OAuth consent screen):

1. **Branding**: app name `things-hub`, your email as the support and developer contact. Save.
2. **Audience**: user type **External**. Then click **Publish app**, so the status is **In production**.
   - Leave it In production. In **Testing**, Google expires the login after 7 days and the routine stops every week.
   - The app stays unverified. That is fine for personal use (under 100 users). At login Google shows a "Google hasn't verified this app" warning once. Click **Advanced → Go to things-hub (unsafe)** to continue.
3. **Data access**: adding scopes here is optional. The login asks for them itself, all read-only:
   - `classroom.courses.readonly`
   - `classroom.coursework.me.readonly`
   - `classroom.student-submissions.me.readonly`
   - with `thub-classroom login --announcements`, also `classroom.announcements.readonly` (no event uses it yet)

## 3. Create the Desktop client

1. **Google Auth Platform → Clients → Create client**, application type **Desktop app**, name `things-hub-mac`.
2. Copy the **Client ID** and **Client secret**. You can see the secret again later on the client's page.

A Desktop client accepts any `http://127.0.0.1:<port>` redirect, so there is no redirect URI to register.

## 4. Fill in `.env` on the Mac

In the repository root:

```sh
cp .env.example .env
chmod 600 .env
```

Then edit `.env`:

| Key | Value |
| --- | --- |
| `THUB_URL` | the inbox, `http://127.0.0.1:8787` over the SSH forward |
| `THUB_TOKEN` | the Mac sender's token (`thub_…`) |
| `THUB_GOOGLE_CLIENT_ID` · `THUB_GOOGLE_CLIENT_SECRET` | from step 3 |
| `THUB_CLASSROOM_ACCOUNT` | a short alias for the Classroom account, such as `university`. It becomes the event `account` (`classroom:university:assignment:…`), so keep it stable |
| `THUB_CLASSROOM_REFRESH_TOKEN` | leave empty; the login writes it |

`.env` is gitignored, and both tools refuse it if group or others can read it. Never commit it, and never put these values in the Keychain or the repository.

On the home server, allow the alias on the Mac's sender:

```sh
docker exec things-hub thub-server sender allow mac --source classroom --account university
```

## 5. First login

```sh
make classroom-login
```

This builds `bin/thub-classroom` and runs `bin/thub-classroom login`. The login uses PKCE and a loopback redirect on `127.0.0.1`.

1. Your browser opens Google's sign-in page.
2. **Pick the Google account that holds Classroom** (probably the university account). The account is chosen here, not in any setting.
3. Accept the unverified-app warning (step 2), then allow the read-only access.

The helper stores the refresh token in `.env` and checks it with a first read: `Classroom answered: N active course(s).` To switch accounts later, run the login again; it replaces the token.

If the browser does not open, use `bin/thub-classroom login --no-browser` and open the printed URL yourself.

### If the school blocks the app

A school Google Workspace can block third-party apps or the Classroom API for students. You see it at once: Google shows **"Access blocked"** or **"…has been blocked by your administrator"** (`admin_policy_enforced`), or the first read fails with `PERMISSION_DENIED`. Then this route cannot work for that account. The fallbacks, from the wiki README:

- Classroom's notification emails (new assignment, due soon) in the school Gmail.
- Each class's Google Calendar, which lists the due dates.

Both are less precise: neither shows whether you already turned the work in.

## 6. Try one run, then install the routine

```sh
make classroom-run        # one run now, in the foreground
make classroom-install    # the launchd job, every hour while the Mac is awake
```

- `make classroom-install CLASSROOM_INTERVAL=1800` changes the interval, in seconds (at least 300). Run it again to change the interval later.
- The job runs once at install and at each login. A run missed while the Mac sleeps happens once on wake.
- One run is limited to 20 minutes (`CLASSROOM_RUN_TIMEOUT`, in seconds).
- The log is `~/Library/Logs/things-hub/classroom.log`. It holds Claude's report: counts only, no titles.
- `make classroom-uninstall` stops and removes the job.

The run starts `claude -p` with `--permission-mode dontAsk` and allows only these tools:

- `bin/thub-classroom assignments`
- `thub-send flush`, `thub-send open classroom …`, and `thub-send post` for files in the run's temporary directory
- `date -u`
- Read inside `skills/things-hub-classroom/` and the temporary directory, and Write inside the temporary directory

Anything else is refused without asking.

## What the events look like

The skill sends one `assignment` event per open coursework:

- `source_id` is `<courseId>/<courseWorkId>`.
- The revision is the later of the coursework's and your submission's update time and, when the item is not open in the inbox, the run's start time (so work that comes back after a resolve is queued again); an open item never goes below the revision the inbox already has.
- The event carries the due date, the link and a short summary.

Old work is filtered: work not open in the inbox is left out when it was due (or, without a due date, created) more than 14 days ago and neither it nor your submission changed in those 14 days.

Turning in a submission resolves its item, and so does a returned one. The on-demand run can then complete the to-do. An item also resolves when its course is archived or the work is removed. If you unsubmit, the next run sends the assignment again.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `Google refused the refresh token` | Run `make classroom-login` again. It happens when access was removed at myaccount.google.com/permissions, when the app was left in Testing, or when the account's admin revoked it |
| thub-send exit 2 with HTTP 403 for the cursor | The Mac's sender is not allowed `classroom/<alias>`; run the `sender allow` command in step 4 |
| thub-send exit 75 | The inbox is unreachable (is the SSH forward up?). The batch waits in the spool and goes out with the next run |
| `claude is not on PATH` in the log | The job's PATH covers `~/.local/bin`, `/opt/homebrew/bin` and `/usr/local/bin`; if `claude` lives elsewhere, edit `launchd/things-hub-classroom.plist.in` and reinstall |
| The run asks for a login | Log in to Claude Code once in a terminal (`claude`), with the Team account |
