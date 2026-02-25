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

Downloads the binary, example config, and quick-start README into the current directory.

## Quick Start

```bash
cp golinx.example.toml golinx.toml
# Edit golinx.toml — add at least one listener (e.g. http://:8080)
./golinx
```

Or build from source:

```bash
go build -o golinx . && ./golinx --listen "http://:8080"
```

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
| [In-App Help](docs/app-help.md) | Quick reference for using the UI — search, shortcuts, themes, permissions |
| [Admin Guide](docs/admin-guide.md) | Configuration, listener URIs, HTTPS, permissions, API, development setup |
| [Tailscale Grants](docs/tailscale-grants.md) | Step-by-step setup for admin access via Tailscale ACL grants |

In-app help is also available at `/.help` or by pressing **F1**.

## License

BSD 3-Clause. See [LICENSE](LICENSE).
