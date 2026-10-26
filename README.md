# jendapp-core

Server for jendapp, a personal "everything app". It runs Claude Code headless inside module folders, and the iOS/macOS client lives in [jendapp-app](https://github.com/jkrecek/jendapp-app) (TestFlight only, single user).

## Concepts
- **Module** — any folder with a `jendapp.toml` (and usually a `CLAUDE.md`). Module code stays in its own folder/repo; jendapp only integrates.
- **Inbox** — files shared from the app are stored, matched against each module's `[[import]]` rules (config order, first match wins) and copied into the module. Unmatched files wait for manual assignment (copied to `<module>/inbox/`).
- **on_import** — after a debounce window, one `claude -p` run per module gets the batch of imported paths.
- **Chat** — `claude -p` per message in the module folder, `--resume` for continuity. Sessions live in `~/.claude/projects/…`, so they're shared with terminal `claude --resume`.
- **Actions** — `[[actions]]` (`title`, `icon`, `prompt`) appear as buttons on the module screen; each starts a new chat with that prompt. `input = "month"` makes the app ask for a month first and fills `{month}` (YYYY-MM); `[actions.confirm]` (`title`, `prompt`) is offered as a button once the run finishes (e.g. Clockify's "Send to Clockify").
- Headless runs use `--permission-mode dontAsk`: only the module's `allowed_tools` are usable; denials are recorded on the job. An appended system prompt lists the allowed tools and asks for one plain Bash command per call.
- `[claude] add_dirs` (relative to the module dir) are passed as `--add-dir`; import `dest` may point into them (e.g. Taxenda → `../faktury/jendapp`).
- Modules today: Marathon (iCloud `Marathon/`, example manifest in `modules/marathon/`), Taxenda (iCloud `Business/taxenda`), Clockify (`~/dev/personal/clockify-exporter`). Their `jendapp.toml` lives in those folders.

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
| GET | `/api/jobs[/{id}]` | jobs (`?module=`) |
| GET | `/api/jobs/{id}/events` | SSE: Claude stream-json lines, then `event: end` with the job |

## Deploy (this Mac)
- `~/.jendapp/config.toml`, binary at `~/.jendapp/bin/jendapp`, logs in `~/.jendapp/logs/`
- `deploy/install-macos.sh` builds, signs with your Apple Development identity (stable identifier `dev.jendapp.server`, so Full Disk Access / iCloud grants survive rebuilds), installs and (re)starts the launchd agent `deploy/dev.jendapp.server.plist`
- Grant `~/.jendapp/bin/jendapp` Full Disk Access once (modules may read e.g. the Calendar DB)
- HTTPS on the tailnet: `tailscale serve --bg --https=8443 http://127.0.0.1:8787` → `https://<host>.<tailnet>.ts.net:8443`
- Linux: `deploy/jendapp.service` (systemd user unit)
