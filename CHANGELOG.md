# Changelog

All notable changes to gotify-commander. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- Saving the configuration in the Gotify UI no longer resets the command
  count, the last command and the uptime shown on the plugin page and by
  `/health`. The stream listener keeps its connection when `server_url`,
  `client_token` and `command_app_id` did not change; it reconnects only when
  one of them did.

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
