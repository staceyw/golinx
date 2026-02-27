# GoLinx

URL shortener and people directory in a single Go binary. Type `go/jira` instead of hunting for a bookmark — short links redirect instantly and people linx get automatic profile pages. Everything runs from one embedded SPA with SQLite storage. Supports HTTP, HTTPS, and Tailscale listeners.

![screenshot](docs/screenshot.svg)

## Install

**Linux / macOS:**

```bash
curl -fsSL https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.sh | bash
```

**Windows (PowerShell):**

```powershell
iex (irm https://raw.githubusercontent.com/staceyw/GoLinx/main/scripts/install.ps1)
```

Downloads the binary and example config into the current directory.

## Quick Start

```bash
./golinx --listen "http://:80"
```

Or build from source:

```bash
go build -o golinx . && ./golinx --listen "http://:80"
```

Open `http://localhost` — done. For persistent configuration, copy `golinx.example.toml` to `golinx.toml` and run `./golinx` with no flags.

> Port 80 is required for `go/link` to work in the browser. On Linux, use `sudo` if port 80 is restricted. For Tailscale, add both listeners so the FQDN works too: `--listen "ts+https://:443" --listen "ts+http://:80"`. See [Making `go/link` Work](docs/admin-guide.md#making-golink-work) for the full explanation of HTTP vs HTTPS and bare hostnames vs FQDNs.

## Highlights

- **Links + People** — short links, employees, customers, and vendors in one unified grid
- **Fuzzy search** with type prefix filters (`:e`, `:c`, `:v`, `:l`) and tag search (`:t`)
- **12 themes** — Catppuccin Mocha, Dracula, Nord, Solarized, Gruvbox, and more
- **Profile pages** with avatar, contact info, and social links
- **Path passthrough** — `/github/org/repo` resolves through `/github` to the full URL
- **Go template URLs** — `{{.Path}}`, `{{.User}}`, `{{.Query}}` for dynamic routing
- **Tailscale integration** — runs on your tailnet via tsnet with automatic HTTPS and user identification
- **Single binary** — all HTML/CSS/JS/help embedded, no external assets or build tools

## Linx Types

| Type | Badge | `/{name}` behavior |
|------|-------|-----------------------|
| Link (default) | — | 302 redirect to destination URL |
| Employee | Emp | Profile page |
| Customer | Cus | Profile page |
| Vendor | Ven | Profile page |

## Documentation

| Guide | Description |
|-------|-------------|
| [Admin Guide](docs/admin-guide.md) | Configuration, listener URIs, HTTP vs HTTPS, permissions, API reference, development setup |
| [In-App Help](docs/app-help.md) | Quick reference for using the UI — search, shortcuts, themes, sorting, views, tags |
| [Tailscale Grants](docs/tailscale-grants.md) | Step-by-step setup for admin access via Tailscale ACL groups, node tagging, and grants |
| [Destination URL Templates](docs/dest-url-help.md) | Go template syntax for dynamic URLs — path passthrough, query parameters, built-in functions |

In-app help is also available at `/.help` or by pressing **F1**.

## License

BSD 3-Clause. See [LICENSE](LICENSE).
