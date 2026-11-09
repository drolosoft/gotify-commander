# Security

gotify-commander runs real system commands on the machine that hosts Gotify
and, through SSH, on the machines you configure. Treat its deployment with
the same care as an SSH account.

## Reporting a vulnerability

Report security problems in private, not in a public issue: open a private
security advisory on GitHub (Security tab, "Report a vulnerability").

Include the plugin version (`go version -m gotify-commander.so` prints it),
the Gotify version and the steps to reproduce. Fixed issues are listed in
`CHANGELOG.md` under Security.

## Scope

In scope: the plugin code in this repository (`plugin.go` and `internal/`),
the web control panel it serves, the way it talks to Gotify and to SSH
targets, and the build scripts that produce the released `.so` files.

Out of scope: Gotify itself, the operating system, nginx, sudo rules you
configure, and the Gotify client apps.

## Threat model

Who can make the plugin run a command:

1. Anyone who can post a message to the Gotify application configured as
   `command_app_id`. On Gotify 3 any client token of the same user can post
   with an `appid`, so every client token of the admin account is a command
   token.
2. Anyone who knows the control panel password (`web_password`, or the
   generated one shown on the plugin page in Gotify).
3. Only Gotify administrators can enable the plugin. For other users the
   plugin is a stub that shows a notice and registers nothing.

Commands run with the permissions of the Gotify process plus the `sudo` rules
you grant it. Only the 27 registered commands can run, and service names are
validated against a whitelist.

Arguments are never joined into a shell string. The one fixed shell script in
`traffic` gets its inputs as positional arguments. SSH host keys are verified
against `known_hosts`, and every command has a timeout that kills the whole
process group.

## What the control panel requests from other hosts

The panel's page is one response with its stylesheet, scripts and icons
inside, and its API is on the same origin. The browser that shows it asks two
other kinds of host for an image:

- each service's own `domain`, for its favicon, with no `Referer`;
- `tile.openstreetmap.org`, for the map tile at the end of `locate`. This
  request carries the origin of your Gotify as `Referer` (scheme and host),
  because that server refuses a browser that sends none. The panel's path,
  which holds the plugin token, is never sent.

An image from any other host in a command's answer is shown as a link and is
not requested. See [The control panel](doc/control-panel.md).

## Hardening

- Keep Gotify behind a VPN or a reverse proxy with its own authentication.
- Grant `sudo` only for the binaries the commands need (see the commented
  block at the end of `config.example.yaml`).
- Use the generated panel password or a long `web_password`, and keep it out
  of chat and screenshots.
- Create as few Gotify client tokens as possible on the admin account.
