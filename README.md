# jendapp-core

Server for jendapp, a personal "everything app". It runs Claude Code headless inside module folders, and the iOS/macOS client lives in [jendapp-app](https://github.com/jkrecek/jendapp-app) (TestFlight only, single user).

## Concepts
- **Module** — any folder with a `jendapp.toml` (and usually a `CLAUDE.md`). Module code stays in its own folder/repo; jendapp only integrates.
- **Inbox** — files shared from the app are stored, matched against each module's `[[import]]` rules (config order, first match wins) and copied into the module. Unmatched files wait for manual assignment (copied to `<module>/inbox/`).
- **on_import** — after a debounce window, one `claude -p` run per module gets the batch of imported paths.
- **Chat** — `claude -p` per message in the module folder, `--resume` for continuity. Sessions live in `~/.claude/projects/…`, so they're shared with terminal `claude --resume`.
- **Actions** — `[[actions]]` (`title`, `icon`, `prompt`) appear as buttons on the module screen; each starts a new chat with that prompt. `input = "month"` makes the app ask for a month first and fills `{month}` (YYYY-MM); `[actions.confirm]` (`title`, `prompt`) is offered as a button once the run finishes (e.g. Clockify's "Send to Clockify").
- **Views & operations** (preferred over chat) — a module declares what it shows and what you can do, and the app draws it from fixed building blocks; Claude is one kind of operation, and free chat is the fallback:
  - `[[sources]]` `run = [argv…]` (no shell, in the module dir) prints `{"jendapp":1,"rows":[{"id":…},…],"summary":{…},"notes":[…]}`. The last good output is kept in SQLite and served (marked `stale`) when a refresh fails.
  - `[[views]]` (`type = "table"`) bind a source to `columns` (`type` text|number|hours|date|enum, `role` title|subtitle|caption|warning|trailing|badge, enum `options` with label/color, `fallback` column), `stats` (summary tiles), `group_by`, `filter`, and the operations offered as `actions` (view) and `row_actions` (row).
  - `[[operations]]` are `kind = "command"` (`run`, answers when finished; output is the message) or `kind = "claude"` (`prompt`, opens as a chat). `scope = "row"` operations get `{row.<field>}` from the server's last snapshot, never from the client; `when` limits them by row values; `inputs` ask for text (`{input.<key>}`); `confirm` asks first. `{month}` (YYYY-MM) is the only param so far. A module's command operations run one at a time and appear in Activity as `op` jobs.
  - Example: Clockify's review table (`~/dev/personal/clockify-exporter/jendapp.toml`).
  - A command operation may declare `next = [ids]`: printing a line `jendapp:next <id>` makes the server start that operation once the command succeeds (only ids listed in `next`; the line isn't shown to the user). Used to hand a code pipeline over to a Claude browser step only when there's something to do (Investments' Sync).
  - `[[schedules]]` `op`, `at = "HH:MM"` runs a module command operation daily (local time; catches up after downtime, once per day).
- Headless runs use `--permission-mode dontAsk`: only the module's `allowed_tools` are usable; denials are recorded on the job. An appended system prompt lists the allowed tools and asks for one plain Bash command per call.
- `[claude] add_dirs` (relative to the module dir) are passed as `--add-dir`; import `dest` may point into them (e.g. Taxenda → `../faktury/jendapp`).
- `[claude] chrome = true` passes `--chrome`, so Claude can drive the signed-in Chrome on this Mac for sites without an API (Investments → Yahoo / Google Finance). Its tools still need `"mcp__claude-in-chrome"` in `allowed_tools`, and Chrome must be running with the extension connected.
- Modules today: Marathon (iCloud `Marathon/`, example manifest in `modules/marathon/`), Taxenda (iCloud `Business/taxenda`), Clockify (`~/dev/personal/clockify-exporter`), Investments (`~/dev/personal/investments`). Their `jendapp.toml` lives in those folders.

## Server
```sh
cd server
go build -o bin/jendapp ./cmd/jendapp
cp config.example.toml ~/.jendapp/config.toml   # edit
bin/jendapp pair iphone                         # prints a device token once
bin/jendapp serve
```
Cross-compile for Linux: `GOOS=linux GOARCH=amd64 go build ./cmd/jendapp`.

## API (Bearer token)
| Method | Path | |
|---|---|---|
| GET | `/api/modules` | modules |
| POST | `/api/inbox` | multipart, field `file` (repeatable) |
| GET | `/api/inbox?status=pending` | inbox items |
| POST | `/api/inbox/{id}/assign` | `{"module_id"}` |
| POST | `/api/modules/{id}/chat` | `{"message","session_id"?}` → job |
| GET | `/api/modules/{id}/sessions[/{sid}]` | Claude sessions / messages |
| GET | `/api/modules/{id}/views/{view}?month=` | source rows + summary (`stale`/`error` if served from cache) |
| POST | `/api/modules/{id}/ops/{op}` | `{"view","params","row_id"?,"inputs"?}` → job (command: when finished; claude: 202, stream it) |
| GET | `/api/jobs[/{id}]` | jobs (`?module=`) |
| GET | `/api/jobs/{id}/events` | SSE: Claude stream-json lines, then `event: end` with the job |

## Deploy (this Mac)
- `~/.jendapp/config.toml`, binary at `~/.jendapp/bin/jendapp`, logs in `~/.jendapp/logs/`
- `deploy/install-macos.sh` builds, signs with your Apple Development identity (stable identifier `dev.jendapp.server`, so Full Disk Access / iCloud grants survive rebuilds), installs and (re)starts the launchd agent `deploy/dev.jendapp.server.plist`
- Grant `~/.jendapp/bin/jendapp` Full Disk Access once (modules may read e.g. the Calendar DB)
- HTTPS on the tailnet: `tailscale serve --bg --https=8443 http://127.0.0.1:8787` → `https://<host>.<tailnet>.ts.net:8443`
- Linux: `deploy/jendapp.service` (systemd user unit)
