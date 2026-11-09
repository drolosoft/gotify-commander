# Upgrading from Gotify 2.x

This page is for a server that ran gotify-commander 0.2.0 on Gotify 2.x and
moves to Gotify 3 with plugin 0.3.0 or newer. Do the steps in this order: the
plugin file first, then the tokens, the phone, the panel and the SSH targets.
The full list of changes is in [CHANGELOG.md](../CHANGELOG.md).

## 1. Use the plugin file for your Gotify version

A Go plugin loads only in the Gotify release it was built for. Release 0.2.0
had one file, `gotify-commander-linux-amd64.so`, and it loads only in Gotify
2.6.x. From 0.3.0 on, every release has one file per Gotify version and
architecture:

- Gotify 3 (3.0.x and 3.1.1): `gotify-commander-linux-<arch>-for-gotify-3.1.1.so`
- Gotify 2.9.1: `gotify-commander-linux-<arch>-for-gotify-2.9.1.so`

`<arch>` is `amd64`, `arm64` or `arm-7`. The table is in the
[Compatibility](../README.md#compatibility) section of the README, and any
other Gotify version needs its own build (see
[Building from Source](../README.md#-building-from-source)).

1. Download the file from [Releases](https://github.com/drolosoft/gotify-commander/releases)
   with its `.sha256` and check it: `sha256sum -c <file>.sha256`.
2. Replace `gotify-commander-linux-amd64.so` in the plugin directory with the
   new file (usually `/opt/gotify/data/plugins/`, or the `plugins` folder of
   the Docker data volume). In Docker, change the mounted file and pin the
   image to the same version, for example `gotify/server:3.1.1`.
3. Restart Gotify. If the file does not match the server, the Gotify log
   shows a plugin load error.

## 2. Tokens

Gotify 3 shows a client or application token only once, when it is created.
After that the WebUI does not show it again, so copy it at that moment.

- `client_token`: the plugin needs one client token to read the command
  stream. To create a new one, open the Gotify WebUI, **Clients** ->
  **Create client**, create it without an inactivity expiry (the default),
  copy the token and paste it into the plugin configuration. Gotify deletes
  clients with an expiry when they go unused, and the plugin then loses its
  stream.
- `response_app_token`: no longer used. Leave it empty.
- On Gotify 3 the phone app posts commands with your client token and the
  app ID (`appid`), so no application token is needed for commands. Every
  client token of the account can post into the command app: create as few
  as you need.

## 3. Android app

The Gotify Android app must be 2.10.1 or newer to talk to Gotify 3. Update
it before you send the first command.

## 4. Control panel password

The control panel is locked by default. When `web_password` is empty, the
plugin generates a password when it is enabled and shows it only on its page
in the Gotify UI: **Plugins** -> **gotify-commander**, under **Control panel
password**. It is not written to the log or sent in a message. Set
`web_password` to use your own; empty it to go back to the generated one.

The `?token=` query parameter no longer works. A bookmark with `?token=`
still opens the panel page, which then asks for the password. Scripts that
call the panel API must send the password as a Bearer token:

```bash
curl -H "Authorization: Bearer <password>" "<panel link>/health"
```

The panel link is on the same plugin page.

## 5. SSH targets need known_hosts

The plugin verifies the host key of every SSH target and has no fallback to
an unverified connection. Add the key to the `known_hosts` file of the user
that runs Gotify, or to the file set in `known_hosts_file` for that target,
then check the fingerprint:

```bash
ssh-keyscan -p <port> <host> >> ~/.ssh/known_hosts
```

Until then, commands to that machine fail with a host key error.

## 6. Replay window and rate limit (0.4.0)

From 0.4.0, a command posted while the plugin was not connected to the Gotify
stream (during a Gotify restart, for example) runs once when the plugin
connects again, if it is newer than `defaults.replay_window`: 5m by default,
and `0s` turns the replay off. An older command is not run and the reply says
it expired; on the first start after the upgrade nothing in the history runs.
Commands sent as Gotify messages are limited to 20 per minute per plugin
instance (`defaults.rate_limit_per_minute`), and the control panel keeps its
own 10 per minute.

## Check the upgrade

Send `help` from the phone and open the control panel from the plugin page.
If nothing answers, read the Gotify log for a plugin load error or a host
key error first.
