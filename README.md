<p align="center"><img src="https://gotify.net/img/logo.png" alt="gotify-commander" width="100"></p>

<h1 align="center">gotify-commander</h1>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white" alt="Go"></a>
  <a href="https://gotify.net"><img src="https://img.shields.io/badge/Gotify-2.x-blue" alt="Gotify"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://github.com/drolosoft/gotify-commander/releases"><img src="https://img.shields.io/github/v/release/drolosoft/gotify-commander" alt="GitHub Release"></a>
</p>

> **The first and only bidirectional Gotify plugin.** 24 commands, web control panel, multi-machine — manage your servers from your phone.

Every existing Gotify plugin is one-way — forward to Telegram, Slack, or email. gotify-commander closes the loop: type a command on your phone, get the result as a notification. No bots, no third-party services, no new infrastructure.

---

## How It Works

```
Phone (Gotify app)
      |
      |  "restart nginx"
      v
  Gotify Server  <------------------------------+
      |                                          |
      v                                          |
gotify-commander plugin                    Web UI (Pico CSS)
      |                                    browser dashboard
      +-- local: systemctl restart nginx   (VPS services)
      +-- SSH:   launchctl unload/load     (Mac services)
                            |
                            v
                      Response sent back
                            |
                            v
                  Phone notification
                  "Restart Nginx -- restarted on VPS (:80)"
```

Commands and responses flow through a **single unified Gotify app** — no separate channels to manage. Send a command, get the reply as the next notification.

---

## Quick Start

1. Download the `.so` for your architecture from [Releases](../../releases)
2. Drop it in Gotify's plugin directory (usually `/opt/gotify/data/plugins/`)
3. Restart Gotify: `sudo systemctl restart gotify`
4. Open the Gotify WebUI -> **Plugins** -> **gotify-commander** -> **Configure** -> paste your YAML
5. Enable the plugin
6. Send `help` from the Gotify app on your phone

Access the web control panel at `https://your-gotify-domain/plugin/gotify-commander/`.

---

## Command Reference

### Service Management

| Command | Args | Description |
|---------|------|-------------|
| `status` | `[service]` | Status of all services, or a specific one |
| `restart` | `<service>` | Restart a service (supports aliases) |
| `start` | `<service>` | Start a service |
| `stop` | `<service>` | Stop a service |
| `logs` | `<service>` | Tail the last N lines of service logs |
| `env` | `<service>` | Show environment summary for a service |
| `services` | -- | List all managed services grouped by category |

### System Diagnostics

| Command | Args | Description |
|---------|------|-------------|
| `free` | `[vps\|mac]` | Memory usage |
| `df` | `[vps\|mac]` | Disk usage |
| `uptime` | `[vps\|mac]` | System uptime + load average |
| `top` | `[vps\|mac]` | Top processes by CPU/memory |
| `who` | `[vps\|mac]` | Currently logged-in users |
| `reboot` | `<vps\|mac>` | Reboot a machine (requires explicit target) |

### Network & Analytics

| Command | Args | Description |
|---------|------|-------------|
| `ip` | `[vps\|mac]` | Public IP address of a machine |
| `ports` | `[vps\|mac]` | Open listening ports |
| `cert` | `<domain>` | TLS certificate expiry check |
| `dns` | `<domain>` | DNS lookup |
| `curl` | `<url>` | HTTP health check for a URL |
| `traffic` | `[vps\|mac]` | Live traffic analysis via rhit |
| `analytics` | `[service]` | GoAccess web analytics summary |
| `locate` | `<device>` | GPS locate a device (phone, laptop, etc.) |

### General

| Command | Args | Description |
|---------|------|-------------|
| `help` | -- | Show all commands and configured services |
| `ping` | `[vps\|mac]` | Health check — responds with "pong" + latency |
| `version` | -- | gotify-commander version + Go runtime info |

**Universal aliases:** `mem`/`memory` -> `free` -- `disk`/`space` -> `df` -- `log` -> `logs` -- `up` -> `uptime`

Typing just a service name (e.g. `nginx`) is treated as `status nginx`.

---

## Web UI Control Panel

A browser dashboard built with **Pico CSS** lives at `https://your-domain/plugin/gotify-commander/`.

- Live service status grid — all services, grouped by category, color-coded
- One-click restart / start / stop for any service
- Command history with timestamps and results
- System metrics (memory, disk, uptime) at a glance
- Dynamic favicons fetched from service domains
- Dynamic categories from your YAML config — no hardcoding

Access is secured through your existing Gotify login. No extra credentials.

---

## Configuration

Configure via Gotify WebUI — no config files to edit manually. See [`config.example.yaml`](config.example.yaml) for the full structure.

```yaml
gotify:
  server_url: "http://localhost:2006"
  base_url: "https://gotify.example.com"   # public URL for web UI links
  client_token: "your-client-token"        # from Gotify -> Clients
  command_app_id: 1                        # ID of the unified command/response app
  response_app_token: "your-token"         # token for the same app

defaults:
  timeout: 30s
  log_lines: 30

categories:
  web:
    label: "Web"
    type: web
  media:
    label: "Media"
    type: media
  monitoring:
    label: "Monitoring"
    type: monitoring
  data:
    label: "Data"
    type: data
  automation:
    label: "Automation"
    type: automation
  system:
    label: "System"
    type: system

services:
  nginx:
    description: "Web Server"
    category: web
    domain: "example.com"       # used by 'cert' and 'dns' commands
    machine: vps
    port: 80
    aliases: [ng, web]
    systemd: nginx

  homeserver:
    description: "Home Server"
    category: media
    machine: mac
    port: 8080
    aliases: [home]
    launchd: com.myapp.service
```

Required fields per machine type:
- `machine: vps` — requires `systemd` (unit name for `systemctl`)
- `machine: mac` — requires `launchd` (label for `launchctl`) + SSH target configured

---

## Categories

Services are grouped into categories defined in your YAML config. The 6 default categories:

| Category | Typical services |
|----------|-----------------|
| `web` | nginx, caddy, traefik |
| `media` | Jellyfin, TubeArchivist, Plex |
| `monitoring` | Grafana, Prometheus, Uptime Kuma |
| `data` | PostgreSQL, Redis, Meilisearch |
| `automation` | n8n, Home Assistant, cron jobs |
| `system` | SSH daemon, Fail2ban, Gotify itself |

Categories drive both the `services` command output and the web UI grouping. Add your own by extending the `categories` section.

---

## Multi-Machine

gotify-commander supports two machine types out of the box:

**VPS** — commands run locally on the same server where Gotify is installed. Uses `systemctl` for service management.

**Mac** — commands are sent over SSH to a remote Mac Mini or MacBook. Uses `launchctl` for service management.

```yaml
ssh_targets:
  mac:                            # must be named "mac" to auto-wire
    host: "100.x.x.x"            # Tailscale IP recommended
    port: 22
    user: "admin"
    key_file: "/home/admin/.ssh/id_rsa"
```

Once configured, `free mac`, `df mac`, `uptime mac`, and services with `machine: mac` all route through SSH automatically.

---

## Security

- **Tailscale network** — both VPS and Mac run on a Tailscale VPN; all SSH and HTTP traffic is encrypted peer-to-peer, never exposed to the public internet
- **Gotify login** — web UI is gated behind your existing Gotify authentication
- **Random plugin token** — Gotify generates a unique token per plugin instance
- **Command whitelist** — only registered commands execute; no shell passthrough
- **No shell injection** — all commands go through `exec.Command`, not `sh -c`
- **Input sanitization** — service names must match `^[a-zA-Z0-9_-]+$`, reject everything else
- **Execution timeouts** — configurable (default 30s), prevents runaway processes
- **SSH key-only auth** — password auth not supported; key file path configured explicitly
- **Reboot requires explicit target** — `reboot` alone returns an error, preventing accidental reboots
- **Audit trail** — every command logged: timestamp, command, source, result, duration

---

## Comparison

| Feature | gotify-commander | Telegram Bots | Uptime Kuma |
|---------|:---:|:---:|:---:|
| Self-hosted | Yes | No | Yes |
| Bidirectional | Yes | Yes | No |
| Runs inside Gotify | Yes | No | No |
| No third-party services | Yes | No | Yes |
| Service management | Yes | varies | No |
| Multi-machine (SSH) | Yes | varies | No |
| Web control panel | Yes | No | Yes |
| Traffic & analytics | Yes | No | No |
| GPS locate | Yes | No | No |
| Tailscale security | Yes | No | No |
| Phone app already installed | Yes | Yes | No |

---

## Building from Source

The plugin must be built on Linux with `CGO_ENABLED=1` (Go plugins require cgo).

```bash
git clone https://github.com/drolosoft/gotify-commander
cd gotify-commander
go build -buildmode=plugin -o gotify-commander.so .
sudo cp gotify-commander.so /opt/gotify/data/plugins/
sudo systemctl restart gotify
```

**Using the Makefile:**

```bash
make test          # run unit tests
make lint          # go vet
make deploy        # SSH -> build on VPS -> install -> restart Gotify
make deploy-dev    # fast deploy, skip tests
```

The `deploy` target connects to your VPS via Tailscale, pulls the latest commit, builds in place, copies the `.so`, and restarts Gotify in one step.

---

## Contributing

Contributions are welcome — bug fixes, new commands, feature ideas. Open an issue or submit a PR.

If gotify-commander made your server management easier, consider giving it a star on GitHub — it helps others discover the project.

---

## License

**MIT License** — free to use, modify, and distribute.

**[Drolosoft](https://drolosoft.com)** -- *Tools we wish existed*
