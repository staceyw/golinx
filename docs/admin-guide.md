# GoLinx Admin Guide

Everything you need to install, configure, and operate GoLinx.

## Quick Start

```bash
go build -o golinx .
./golinx --listen "http://:80"
```

Open `http://localhost` — GoLinx starts with an empty database, ready to use.

> **Why port 80?** Short links like `go/jira` only work when the server is on port 80 — that's the default port browsers use for HTTP. If you use port 8080, users would have to type `go:8080/jira`. On Linux, use `sudo` if port 80 is restricted. See [Making `go/link` Work](#making-golink-work) for the full explanation.

## Linux Service (systemd)

To install GoLinx as a background service that starts on boot (e.g. on a Raspberry Pi or Linux server):

```bash
curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install-service.sh | sudo bash
```

The script will prompt you for:

| Prompt | Default | Description |
|--------|---------|-------------|
| Data directory | `/home/<user>/golinx` | Where `golinx.toml` and `golinx.db` are stored |
| Listener URI | `http://:80` | The listener to configure (supports `ts+http://`, `ts+https://`, etc.) |
| Tailscale hostname | `go` | Only prompted if listener is `ts+*` |
| Run-as user | Owner of data directory | The OS user the service runs as |

After install, manage the service with:

```bash
sudo systemctl status golinx       # check status
journalctl -u golinx -f            # view logs
sudo systemctl stop golinx         # stop
sudo systemctl restart golinx      # restart
```

To change settings, edit the config file shown at the end of the install, then `sudo systemctl restart golinx`.

## Proxmox LXC

To create a dedicated LXC container with GoLinx on a Proxmox host:

```bash
curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install-lxc.sh | bash
```

The script creates a Debian 12 container, downloads GoLinx, and starts it as a systemd service. It prompts for container ID, resources, network, and listener configuration.

After install, manage the container from the Proxmox host:

```bash
pct enter <CTID>                                    # shell into container
pct exec <CTID> -- systemctl status golinx          # check service
pct exec <CTID> -- journalctl -u golinx -f          # view logs
pct stop <CTID>                                     # stop container
pct start <CTID>                                    # start container
```

## Listener URIs

Each `--listen` flag takes a self-describing URI. Combine multiple `--listen` flags to run listeners together.

| Scheme | Format | Description |
|--------|--------|-------------|
| `http://` | `http://[addr]:port` | Plain HTTP |
| `https://` | `https://[addr]:port;cert=<path>;key=<path>` | HTTPS with your own certificates |
| `ts+https://` | `ts+https://:port` | Tailscale HTTPS (auto certs, requires `--ts-hostname`) |
| `ts+http://` | `ts+http://:port` | Tailscale plain HTTP (requires `--ts-hostname`) |

Host must be empty or an IP address — hostnames are not allowed in listener URIs.

## Configuration Matrix

| Scenario | Command |
|----------|---------|
| HTTP only on LAN | `./golinx --listen "http://:80"` |
| HTTPS with own certs | `./golinx --listen "https://:443;cert=cert.pem;key=key.pem"` |
| HTTPS + HTTP redirect | `./golinx --listen "http://:80" --listen "https://:443;cert=cert.pem;key=key.pem"` |
| Tailscale HTTPS | `./golinx --ts-hostname go --listen "ts+https://:443" --listen "ts+http://:80"` |
| Tailscale + local LAN | `./golinx --ts-hostname go --listen "ts+https://:443" --listen "ts+http://:80" --listen "http://:80"` |

**Identity:** Tailscale listeners use WhoIs login. Local listeners use `local@<hostname>`. Mixed mode falls back to local identity for non-tailnet requests.

## Config File

Place a `golinx.toml` in the working directory to avoid repeating flags:

```toml
listen = [
  "ts+https://:443",
  "ts+http://:80",
  "http://:80",
]
ts-hostname = "go"
verbose = false
max-resolve-depth = 5
# ts-dir = "/data/tsnet"  # default: OS config dir (e.g. ~/.config/tsnet-golinx)
# user-perms = ["*"]  # LAN user permissions: "add", "update", "delete", or ["*"] for all
```

With a config file, just run `./golinx` — no flags needed.

### CLI and config file merge behavior

GoLinx always reads `golinx.toml` if it exists in the working directory, even when CLI flags are provided. The merge rules:

- **`--listen`** — CLI listeners **replace** config listeners entirely (they are not combined). If no `--listen` flags are given, the config file's `listen` array is used.
- **All other flags** (`--verbose`, `--ts-hostname`, `--ts-dir`, `--max-resolve-depth`, `--user-perms`) — a CLI flag wins only if explicitly set. Otherwise the config file value is used.
- **`--user-perms`** — CLI accepts a comma-separated string (e.g. `--user-perms "add,update"`), config uses an array (e.g. `user-perms = ["add", "update"]`).

If any CLI flags are set and a config file exists, GoLinx prints a warning: `command-line flags override golinx.toml settings`.

This means `./golinx --listen "http://:80"` still picks up `ts-hostname`, `verbose`, `user-perms`, etc. from the config file — it only overrides the listeners.

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | (repeatable) | Listener URI — at least one required |
| `--verbose` | `false` | Verbose tsnet logging |
| `--ts-hostname` | `go` | Tailscale node hostname (required for `ts+*` listeners) |
| `--ts-dir` | OS config dir | Tailscale state directory (e.g. `~/.config/tsnet-golinx`) |
| `--user-perms` | `*` | LAN user permissions: comma-separated `add`,`update`,`delete`, or `*` for all |
| `--max-resolve-depth` | `5` | Maximum link chain resolution depth |
| `--import <file>` | — | Import linx from a JSON backup and exit |
| `--resolve <file> <path>` | — | Resolve a short link from a JSON backup and exit |

## Making `go/link` Work

> **"Why can't I just type `go/jira`?"** — You can, but two things must be true: the name `go` must **resolve** to the server's IP, and the server must be listening on **HTTP port 80**. This section explains both.

### DNS: How `go` resolves to an IP address

Before the browser can connect to anything, the name `go` has to resolve to an IP address. How that happens depends on your setup:

**Tailscale (automatic)** — Tailscale's MagicDNS handles this for you. When you set `ts-hostname = "go"`, every device on your tailnet can resolve `go` to GoLinx's tailnet IP automatically. No DNS configuration needed on any client.

**LAN without Tailscale** — There is no magic. You need to make `go` resolve to the server's LAN IP yourself. Options:

| Approach | Scope | Setup |
|----------|-------|-------|
| **Local DNS server** (Pi-hole, Unbound, AD DNS, router DNS) | Whole network — all devices resolve `go` automatically | Add an A record: `go` → `192.168.1.x` on your DNS server |
| **Router DNS** — many home routers let you add custom DNS entries | Whole network | Router admin panel → DNS / hostname mapping |
| **Hosts file** (`/etc/hosts` or `C:\Windows\System32\drivers\etc\hosts`) | Single machine only | Add `192.168.1.x go` to each machine's hosts file |

Local DNS is the only practical option for more than a couple of machines. Hosts files don't scale — every client needs a manual entry, and any IP change means updating all of them.

> **Tip:** If you can't set up DNS right away, `http://192.168.1.x/jira` always works — but at that point you're typing an IP address, which defeats the purpose of short links.

### HTTPS: Why `go/link` requires HTTP

> **"Why not just use HTTPS?"** — This is the most common question from new deployments. Short answer: you **need** HTTP for `go/link` to work. HTTPS cannot serve bare hostnames, and this is not a GoLinx limitation — it's how TLS certificates work.

1. **Certs require a domain** — TLS certificates (Let's Encrypt, Tailscale, any CA) are issued for fully-qualified domain names like `go.example.ts.net`. They cannot be issued for a bare name like `go`. This is a fundamental PKI constraint — no CA will sign a certificate for a single-label hostname. There is no server-side setting, whitelist, or SAN entry that can override this.

2. **Browsers try HTTPS first** — When you type `go/jira`, modern browsers attempt `https://go/jira`. The server has no valid cert for `go`, so the TLS handshake fails. The browser then falls back to `http://go/jira` on port 80.

3. **Without an HTTP listener, there's nothing to fall back to** — If you only run HTTPS, the fallback fails and the user gets a connection error. The only way to reach the service would be typing `https://go.example.ts.net/jira` every time — defeating the purpose entirely.

### Bare hostname vs FQDN

Browsers treat bare hostnames (no dots) and fully-qualified domain names (with dots) differently:

| What you type | Browser sees | What happens |
|---------------|-------------|--------------|
| `go/link` | Bare hostname (no dot) | Browser tries HTTPS, fails, **falls back to HTTP** on port 80 |
| `go.example.ts.net/link` | FQDN (has dots) | Browser upgrades to HTTPS — **no HTTP fallback** |

This is a browser heuristic, not a server setting. Browsers assume bare names are likely local/intranet and allow HTTP fallback. FQDNs look like "real" internet domains, so browsers enforce HTTPS strictly — if port 443 isn't listening, you get a connection error with no fallback attempt.

### The recommendation

**Always include an HTTP listener** — `go/link` requires it. This is not a security compromise:

- **Tailscale:** Your tailnet traffic is already encrypted by WireGuard end-to-end. HTTPS on top adds no security benefit.
- **LAN:** Traffic stays on your local network.
- **Industry standard:** Every go-link service in production (Google's original, Tailscale's [golink](https://github.com/tailscale/golink), GoLinks SaaS) uses HTTP for the bare hostname.

**Add an HTTPS listener** if you also want the FQDN to work (e.g. for bookmarks or sharing `https://go.example.ts.net/jira`):

| HTTPS listener | Required HTTP listener | Why |
|----------------|----------------------|-----|
| `ts+https://:443` | `ts+http://:80` | `go/link` needs HTTP — the cert only covers the FQDN |
| `https://:443;cert=...;key=...` | `http://:80` | Same — bare hostnames can't use HTTPS |

> **TL;DR:** Run **both HTTP and HTTPS** listeners. HTTP serves `go/link`. HTTPS serves the FQDN. HTTP-only works for bare hostnames but the FQDN will fail on modern browsers. Both together always works.

### Browser-specific notes

- **Chrome** — Handles `go/link` automatically. Auto-upgrades FQDNs to HTTPS (via HTTPS-First mode), so the FQDN requires an HTTPS listener.
- **Firefox** — May treat `go/link` as a search query. Fix: open `about:config` and add `browser.fixup.domainwhitelist.go` as a boolean set to `true`.
- **Safari** — Works automatically on most configurations.

### HTTPS redirect behavior

When an HTTPS listener is active, its corresponding HTTP listener automatically redirects FQDN requests to HTTPS. Bare hostname requests (like `go/link`) are **not** redirected — they are served directly over HTTP so the short link works.

HSTS (`Strict-Transport-Security`) headers are set only for fully-qualified domain names (hostnames containing a dot). This prevents the browser from caching an HSTS policy for `go` — which would permanently break `go/link` access.

> **Note:** If you use `curl` over HTTP when HTTPS is enabled, use the `-L` flag to follow redirects, or FQDN requests will return an empty response.

## Permissions

GoLinx enforces owner-based access control:

| Situation | Edit | Delete | UI |
|-----------|------|--------|-----|
| You own the linx | Yes | Yes | Edit + Delete |
| Linx has no owner | Yes (claims it) | Yes | Edit + Delete |
| Someone else owns it | No | No | View only |
| **Localhost** (127.0.0.1) | **Yes** | **Yes** | **Edit + Delete** |

- Linx are automatically owned by the creating user (Tailscale login or `local@hostname`)
- Unowned linx can be claimed by anyone — editing sets you as owner
- Owners can clear or transfer ownership via the owner field
- Non-owners see a readonly "Linx Info" view
- **Localhost auto-admin** — requests from 127.0.0.1 or ::1 have full access, no toggle needed
- **User permissions** — `user-perms` config controls what non-localhost LAN users can do (`["*"]` default = full access, `["add"]` = create only, `[]` = read-only). Localhost and Tailscale users are not affected
- Enforced server-side — API returns 403 for unauthorized actions

## Admin Mode (Tailscale Grants)

Admin mode lets designated users bypass ownership checks — editing or deleting any linx regardless of owner. Admins see an **Admin** toggle in the header; it must be switched on to take effect.

Admin status is configured via **Tailscale ACL grants**, not in the GoLinx config file. This means admin membership is managed centrally in your Tailscale policy and takes effect immediately — no GoLinx restart needed.

See [tailscale-grants.md](tailscale-grants.md) for step-by-step setup instructions covering group creation, node tagging, and grant configuration.

## Link Resolution

When a request arrives at `/{name}` (or `/{name}/extra/path`):

1. **Lookup** — the first path segment is used as the short name
2. **Punctuation trim** — if not found, trailing `.,()[]{}` are stripped and retried
3. **Expand** — the destination URL is expanded with any extra path/query via Go templates
4. **Chain follow** — if the expanded URL points to another local link, it is followed recursively (up to `max-resolve-depth` hops, default 5)
5. **Redirect** — the final URL is served as a 302 redirect; each hop increments its click count

### Go Template URLs

Destination URLs support Go `text/template` syntax for advanced routing:

| Variable/Function | Description |
|-------------------|-------------|
| `{{.Path}}` | Extra path after the short name |
| `{{.User}}` | Tailscale user login (or local identity) |
| `{{.Query}}` | Full query string |
| `{{.Now}}` | Current time (UTC) |
| `PathEscape` | URL path-escapes a string |
| `QueryEscape` | URL query-escapes a string |
| `TrimPrefix` | Trims a prefix from a string |
| `TrimSuffix` | Trims a suffix from a string |
| `ToLower` / `ToUpper` | Case conversion |
| `Match` | Regexp match (returns bool) |

Example: a link with destination `https://search.example.com/q={{QueryEscape .Query}}` redirects `/search?foo+bar` to `https://search.example.com/q=foo+bar`.

## Export & Import

### Export

Visit `/.export` to download all linx as `links.json`.

### Import

```bash
./golinx --import links.json
```

Loads linx from a JSON backup into the database. Existing short names are skipped — import is additive only.

### Resolve

```bash
./golinx --resolve links.json github/test
```

Tests link resolution from a JSON backup without starting the server. Loads data into an in-memory SQLite database and runs the same resolution logic as the live server. Useful for verifying redirects before importing.

## API Reference

```
GET    /api/linx              List linx (optional ?type= filter)
POST   /api/linx              Create linx
PUT    /api/linx/{id}         Update linx
DELETE /api/linx/{id}         Delete linx
POST   /api/linx/{id}/avatar  Upload avatar
GET    /api/linx/{id}/avatar  Serve avatar
GET    /api/stats              Click analytics (top links, daily histogram, summary)
GET    /api/settings           Get setting (?key=)
PUT    /api/settings           Save setting
GET    /api/whoami             Current user, hostname, and Tailscale mode
GET    /.addlinx               Open the New Linx dialog
GET    /.help                  In-app help page
GET    /.export                Export all linx as JSON
GET    /.ping/{host}           TCP ping (host or host:port)
GET    /.whoami                WhoIs terminal (Tailscale user/node info)
GET    /{shortname}            Redirect or profile page
GET    /{shortname}+           Detail page (link metadata or profile)
```

## Development Setup

Scripts keep the repo root clean by building and running from `dev/`:

```bash
# 1. Copy the example config into dev/ and edit for your environment
mkdir dev
cp golinx.example.toml dev/golinx.toml

# 2. Build and run (PowerShell: .\scripts\run.ps1)
./scripts/run.sh

# 3. Seed sample data while server is running (PowerShell: .\scripts\seed.ps1)
./scripts/seed.sh http://localhost:8080
```

Runtime files (`golinx.db`, config, binary) all live in `dev/` which is gitignored. Edit `dev/seed.json` to customize seed data.

### Seed Data

GoLinx starts with an empty database. Populate it with sample linx:

```bash
./scripts/seed.sh http://localhost:8080           # local
./scripts/seed.sh https://go.example.ts.net       # tailnet
```

## Architecture

```
main.go              Entry point
golinx.go            HTTP handlers + embedded SPA (HTML/CSS/JS as string literal)
db.go                SQLite data layer with mutex-protected CRUD
schema.sql           Embedded via //go:embed
docs/app-help.md     In-app help (Markdown, embedded and rendered via goldmark)
docs/admin-guide.md  This file — operator/admin documentation
static/favicon.svg   App icon (embedded via //go:embed)
scripts/             Build, seed, and release scripts (PowerShell)
golinx.example.toml  Example configuration file
```

Pure Go with `modernc.org/sqlite`, `tailscale.com/tsnet`, and `github.com/yuin/goldmark` — no CGo, no Node, no build tools.
