# GoLinx Admin Guide

Everything you need to install, configure, and operate GoLinx.

## Quick Start

```bash
go build -o golinx .
./golinx --listen "http://:8080"
```

Open `http://localhost:8080` — GoLinx starts with an empty database, ready to use.

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
| HTTP only on LAN | `./golinx --listen "http://:8080"` |
| HTTPS with own certs | `./golinx --listen "https://:443;cert=cert.pem;key=key.pem"` |
| HTTPS + HTTP redirect | `./golinx --listen "http://:80" --listen "https://:443;cert=cert.pem;key=key.pem"` |
| Tailscale HTTPS | `./golinx --ts-hostname go --listen "ts+https://:443" --listen "ts+http://:80"` |
| Tailscale + local LAN | `./golinx --ts-hostname go --listen "ts+https://:443" --listen "ts+http://:80" --listen "http://:8080"` |

**Identity:** Tailscale listeners use WhoIs login. Local listeners use `local@<hostname>`. Mixed mode falls back to local identity for non-tailnet requests.

## Config File

Place a `golinx.toml` in the working directory to avoid repeating flags:

```toml
listen = [
  "ts+https://:443",
  "ts+http://:80",
  "http://:8080",
]
ts-hostname = "go"
verbose = false
max-resolve-depth = 5
# ts-dir = "/data/tsnet"  # default: OS config dir (e.g. ~/.config/tsnet-golinx)
# user-perms = ["*"]  # LAN user permissions: "add", "update", "delete", or ["*"] for all
```

With a config file, just run `./golinx` — no flags needed. Command-line flags override config file values (with a warning).

## Global Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | (repeatable) | Listener URI — at least one required |
| `--verbose` | `false` | Verbose tsnet logging |
| `--ts-hostname` | — | Tailscale node hostname (required for `ts+*` listeners) |
| `--ts-dir` | OS config dir | Tailscale state directory (e.g. `~/.config/tsnet-golinx`) |
| `--max-resolve-depth` | `5` | Maximum link chain resolution depth |
| `--import <file>` | — | Import linx from a JSON backup and exit |
| `--resolve <file> <path>` | — | Resolve a short link from a JSON backup and exit |

## Why HTTP is Recommended for Tailscale

The whole point of a short link service is typing `go/jira` in your browser's address bar — minimal and fast. This only works over HTTP. Here's why:

Tailscale HTTPS certs are issued for the FQDN (e.g. `go.example.ts.net`), not the bare name `go`. If you only have an HTTPS listener, typing `go/jira` fails — the browser tries HTTPS, the cert doesn't match `go`, and there's nothing to fall back to. You'd have to type `go.example.ts.net/jira` every time, which defeats the purpose.

**Recommended:** Use `ts+http://:80` as your Tailscale listener. Your tailnet traffic is already encrypted by WireGuard, so HTTPS adds no real security benefit. This gives you the clean `go/link` experience.

**Optional:** If you also want HTTPS for the FQDN (e.g. for bookmarking `https://go.example.ts.net`), add both listeners. The HTTP listener catches `go/link` requests, and HTTPS serves the FQDN:

| HTTPS listener | Required HTTP listener | Why |
|----------------|----------------------|-----|
| `ts+https://:443` | `ts+http://:80` | `go/link` falls back to HTTP since the cert only covers the FQDN |
| `https://:443;cert=...;key=...` | `http://:80` | Same fallback behavior for LAN hostnames |

## HTTPS Redirect

When an HTTPS listener exists (`https://` or `ts+https://`), its corresponding HTTP listener (`http://` or `ts+http://`) automatically redirects requests to the HTTPS equivalent. If `ts+https://` is configured but the tailnet does not support HTTPS certificates, GoLinx exits with an error.

HSTS (`Strict-Transport-Security`) headers are set only for fully-qualified domain names (hostnames containing a dot). This avoids HSTS issues with `localhost`, bare hostnames like `go`, and IPv6 addresses.

> **Note:** If you use `curl` to interact with the API over HTTP when HTTPS is enabled, use the `-L` flag to follow redirects, or your request will return an empty response. We recommend always using `-L` regardless of current HTTPS status.

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
