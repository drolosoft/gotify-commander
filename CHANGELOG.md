# Changelog

All notable changes to gotify-commander. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- New command `failed [vps/mac]`: the failed systemd units, or
  "✅ No failed units". On the mac target it first asks the machine its
  system and answers "not available on macOS" there.
- New command `health`: one message with every mount at 85 % or more, the
  load average against the CPU count, the memory in use, the failed units
  and the certificates that expire within 14 days, each line marked ✅ or ⚠️.
  It reads the certificates with the same `find` and `openssl` calls as
  `certs`.
- New command `docker ps|restart|logs <name>`, limited to the containers in
  the new `docker.allowed_containers` setting and only when docker is
  installed. `docker restart` is a dangerous command: one at a time, like
  `reboot`.
- New command `digest`, which sends the health summary now, and the new
  `digest:` section (`enabled`, `at`, `timezone`) that sends it every day at
  that local time.
- New setting `defaults.replay_window`: how old a command posted while the
  plugin was not connected may be and still run when it connects again. 5m by
  default; `0s` turns the replay off. A negative value is refused when the
  configuration is saved. Changing it reconnects the stream listener.
- A command older than the replay window is not run. The reply is
  "⌛ Command expired, not executed: <command>", with only the command word.
- Optional confirmation for commands sent as Gotify messages, off by
  default. List the commands in the new `require_confirm` setting, for
  example `[reboot, shutdown, stop]`, and the first message no longer runs
  them: the reply is "⚠️ Confirm `reboot vps`: reply `confirm 4821` within
  60 s", and only `confirm <code>` runs it. The code is random, works once,
  and only one command waits at a time; a new one replaces it and says so. A
  wrong or expired code gets "❌ No pending command for that code". The
  panel keeps asking in the browser.
- New `confirmation:` section for that reply: `keyword` (the reply word,
  `confirm` by default), `prompt` (the request text, with `{command}`,
  `{keyword}`, `{code}` and `{seconds}`) and `window` (how long a code works,
  60s by default). A keyword that is not one lowercase word or that is also a
  command or service name, a prompt without `{code}` and a window under 1s
  are refused when the configuration is saved.
- New setting `enabled_commands`: the only commands that run, from messages
  and from the panel. Left out or empty, all of them run. Any other command
  gets "🚫 Command disabled by configuration" and is not counted. Unknown
  names in either list are refused when the configuration is saved.
- New setting `ssh_targets.<name>.os`: `linux` (the default) or `darwin`.
  With `os: darwin`, `free`, `top`, `ip`, `ports` and `connections` run
  macOS commands on that machine. Any other value is refused when the
  configuration is saved.
- `logs` works for Mac services: the last `log_lines` lines of the macOS
  unified log of the service's process, from the last 10 minutes.

### Changed

- Commands sent as Gotify messages: the default limit goes from 10 to 20 per
  minute. The panel keeps its 10 per minute.
- New setting `defaults.rate_limit_per_minute` for the commands sent as Gotify
  messages. Empty or 0 means 20; a negative value is refused when the
  configuration is saved.
- The rate limiter moved to the `internal/ratelimit` package. The panel and
  the message path each have their own limiter, so they keep separate
  budgets.
- History entries saved by an earlier version have no timestamp. The panel
  drops them the first time it loads.

### Fixed

- The `mac` target ran Linux commands that macOS does not have: `free -h`,
  `ps --sort`, `hostname -I` and `ss`. With `os: darwin` it runs `vm_stat`
  (shown as free, active, inactive and wired MiB), `ps -r`, `ipconfig
  getifaddr en0` (or the `ifconfig` addresses), `lsof` and `netstat`
  (counted in Go). `updates mac` runs `softwareupdate -l`; `updates` alone
  keeps checking `apt` on the VPS.
- `restart`, `stop`, `start` and `status` of Mac services did not agree:
  `launchctl load` and `unload` need the path of a plist, while `status`
  looked for the label. All of them now use the label from `launchd` in the
  user's GUI domain: `restart` is `launchctl kickstart -k`, `stop` is
  `launchctl bootout`, `start` is `launchctl enable` plus `kickstart -k`,
  and `status` of a service without a port reads `launchctl print`
  (`running (pid N)`, `not running` or `not loaded`). This does not depend
  on `os`, since launchd only exists on macOS. After a `stop` the service is
  unloaded, and `start` answers with launchctl's "Could not find service"
  error until it is loaded again on the Mac (`launchctl bootstrap gui/<uid>
  <plist>`).
- A command posted while the plugin was not connected to the Gotify stream
  (a Gotify restart, a dropped connection) was never run: Gotify does not
  push stored messages to a client that connects later. On every connection
  the plugin now reads the newest 100 messages of the command app and runs,
  oldest first, the ones it has not taken yet. A message that arrives both
  ways runs once.
- The ID of the last command taken is kept in the plugin storage, next to
  the generated password, and saved before the command runs: a `reboot` or a
  restart of Gotify is not run again on the way back up. On the first start
  after the upgrade nothing in the history runs.
- Saving the configuration in the Gotify UI no longer resets the command
  count, the last command and the uptime shown on the plugin page and by
  `/health`. The stream listener keeps its connection when `server_url`,
  `client_token` and `command_app_id` did not change; it reconnects only when
  one of them, or `replay_window`, did.

### Security

- Commands sent as Gotify messages are rate limited: 10 per minute per plugin
  instance by default. Before, any client token of the user could queue 16
  `reboot`s. A command over the limit is not run and gets "⏳ Too many
  commands, try again in N s". The panel keeps its own 10 per minute.
- `reboot`, `shutdown` and every command in a `danger: true` category run one
  at a time, from messages and from the panel. A second one sent while the
  first is still running is refused with "⚠️ Another dangerous command is
  still running". Other commands keep running.
- The panel's command history in `localStorage` expires after 24 hours. The
  password dialog says so and points to the Clear button.

## [0.3.0] - 2026-11-08

Built for Gotify 3.1.1 and 2.9.1, on amd64, arm64 and armv7. The 0.2.0 file
loads only in Gotify 2.6.x; pick the file that matches your server from the
compatibility table in the README.

Upgrade note: SSH targets now need the remote host key in the `known_hosts`
file of the user that runs Gotify (`ssh-keyscan -p <port> <host> >>
~/.ssh/known_hosts`), or the path set in `known_hosts_file` for that target.
Until then, commands to that machine fail with a host key error.

### Security

- The control panel is protected by default. When `web_password` is empty the
  plugin generates a random password on enable, stores it in the plugin
  storage and shows it only on the plugin page in Gotify. It is never logged
  or sent in a message.
- Panel authentication accepts only `Authorization: Bearer`, compared in
  constant time. The `?token=` query parameter is gone.
- `POST /execute` requires `Content-Type: application/json`, limits the body
  to 64 KiB, checks `Origin` and `Sec-Fetch-Site`, runs at most 2 commands at
  a time and 10 per minute per plugin instance.
- Every panel route sends `Content-Security-Policy` with a per-request nonce,
  `X-Frame-Options: DENY`, `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer` and `X-Content-Type-Options: nosniff`. Pico
  CSS is embedded instead of loaded from a CDN.
- The panel URL, which carries the plugin token, is no longer printed by
  `help`, sent in the "Commander Online" message or written to the log.
- Only Gotify administrators get a working plugin instance. Other users see a
  notice and no routes or stream are opened.
- Command timeouts kill the whole process group (`Setpgid` plus a `Cancel`
  that signals the group), so pipelines such as `sleep | cat` stop on time.
  Output is capped at 64 KiB.
- `certs`, `analytics` and `locate` no longer run through `bash -c`. The one
  remaining shell script in `traffic` is fixed text and receives the domain
  and the temp file path as positional arguments.
- SSH host keys are verified against `known_hosts`; `InsecureIgnoreHostKey`
  is gone. Remote commands are shell-quoted word by word.
- The Gotify client token travels in the `X-Gotify-Key` header instead of the
  websocket URL, and every stream log line is passed through a redaction
  helper.
- `locate` validates that latitude and longitude are finite and in range,
  escapes the OpenStreetMap text before building markdown, sets a proper
  `User-Agent` and keeps coordinates out of error messages and logs.
- Panics in a command handler or in the stream goroutines are recovered,
  logged with a stack trace and answered with an "internal error" message
  instead of killing the Gotify process.

### Changed

- The stream read loop only reads. Commands go to a bounded queue (16) and a
  worker runs them, so ping/pong keeps working during long commands such as
  `traffic` and the client is no longer dropped after 60 s. When the queue is
  full the sender gets "busy, try later".
- Reconnection uses exponential backoff from 1 s to 60 s with jitter.
- `Stop` on the listener cancels the context, closes the connection and waits
  for the goroutines. Config changes made in the Gotify UI restart the
  listener and the commander with the new values.
- `response_app_token` is no longer required (it was unused). `command_app_id`
  must be a positive integer and category command names are validated
  against the registry.
- The plugin version comes from `-ldflags "-X main.version=..."` set by the
  Makefile from `git describe`.
- `analytics` parses the goaccess CSV in Go; `python3` is no longer needed.
- `traffic` matches the domain as plain text instead of a regular
  expression.
- The panel asks for the password in its own dialog, keeps it in
  `sessionStorage` for the tab and sends it with every request. A 401 clears
  it and asks again.

### Fixed

- The `traffic` temp file is removed on timeout as well.
- `Listener.conn` was accessed without synchronisation and `Stop` did not
  wait for the read goroutine.
- `esc()` in the panel did not escape quotes although it was used inside
  attributes.
- Removed the development "seed history" entries and `user-scalable=no`
  from the panel.
- `HandleCommand` and `Execute` share one code path.

### Added

- `scripts/pin-gotify.sh <version>` pins `go.mod` to the Gotify server's
  module versions and records them in `.gotify-version` and `.go-version`.
  `scripts/check-plugin-abi.sh` compares a built `.so` with the server's
  `go.mod` and fails on any mismatch.
- `make build-plugin GOTIFY_VERSION=<v> ARCH=<arch>` builds inside the
  official `gotify/build` images; `make build-all` covers the whole matrix.
  The `deploy` targets with placeholder hosts are now a commented example.
- GitHub Actions: `ci.yml` (gofmt, vet, race tests, govulncheck),
  `release.yml` (matrix build, ABI check, checksums, release notes from this
  file with a compatibility table) and `gotify-watch.yml` (weekly check for
  new Gotify releases that opens an issue). Dependabot covers actions only.
- `test/e2e/smoke.sh`: starts Gotify in Docker with the plugin, sends
  `ping`, expects `pong`, checks the panel authentication and a restart.
  Run against 3.1.1 and 2.9.1 on amd64 and arm64 before this release.
- `SECURITY.md` with the reporting process and the threat model.
- Tests for the panel routes, the executor timeouts, the stream queue,
  recovery and shutdown, SSH quoting and host key checks. Root package
  coverage went from 0 % to about 90 %, `internal/stream` from 17 % to
  97 %, `internal/executor` from 37 % to 85 %.

## [0.2.0] - 2026-04-26

Last published release. See the GitHub release notes.
