# GoLinx Help

## Linx Types

Everything in GoLinx is a **Linx**. The linx type determines its behavior:

| Type | Badge | Behavior |
|------|-------|----------|
| Linx | — | Redirects to the destination URL |
| Employee | Emp | Shows a profile page |
| Customer | Cus | Shows a profile page |
| Vendor | Ven | Shows a profile page |

## Creating Linx

- **+** button — creates a new Link (short name + destination URL)
- **Person icon** button — creates a new person linx (Employee, Customer, or Vendor) with name, title, email, phone, and social links
- **Generate** button — generates a random 6-character short code (bit.ly-style) for quick, shareable links

Short names must start with a letter, digit, or underscore, followed by letters, digits, dashes, underscores, or periods. They are unique and case-insensitive.

The label above the Short Name field shows a live preview of the full URL as you type (e.g. `go/jira`). Click it to open the link in a new tab.

## Search

The search box filters linx in real time using substring matching with fuzzy fallback.

**Type prefix filters** narrow results to a single linx type before searching:

| Prefix | Shows | Example |
|--------|-------|---------|
| `:e` | Employees | `:e john` |
| `:c` | Customers | `:c acme` |
| `:v` | Vendors | `:v cal` |
| `:l` | Links | `:l git` |
| `:t` | Tags (OR match) | `:t ops, infra` |

Use the prefix alone (e.g. `:e`) to show all linx of that type.

## Tags

All linx types support comma-separated tags. Use `:t tag1, tag2` to find linx with any of the listed tags.

If your search narrows to exactly one linx, press **Enter** to open it in a new tab.

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Tab` | Cycle focus between search box and linx |
| `Shift+Tab` | Cycle focus in reverse |
| `Enter` | Open focused linx, or single search result |
| `Escape` | Close any open modal or context menu |
| `Ctrl+S` | Save the active modal |
| `Double-click` | Open Edit modal (or Linx Info if not owner) |
| `Right-click` | Open context menu (Edit/Delete or View) |
| `F1` | Open this help page |

Focus is trapped inside open modals — Tab and Shift+Tab cycle through fields without leaving the dialog.

## Sorting

- **A-Z** — alphabetical by short name (links) or first name (people)
- **Popular** — by click count, descending
- **Recent** — by last clicked (links) or date created (people)
- **Charts** — switches to the analytics dashboard (see below)

## Charts

The **Charts** view replaces the grid with a click analytics dashboard:

- **Summary cards** — total links, total clicks, top link, and links created this week
- **Top Links** — horizontal bar chart of the most-clicked links
- **Daily Clicks** — 30-day histogram showing click activity over time

Individual link detail pages (`/{name}+`) also show a 30-day click histogram.

Charts use the ClickLog, which records every redirect. New installations will show data as clicks accumulate.

## Views

Toggle between **Grid** and **List** layout using the view buttons in the toolbar. Your preference is saved.

## Themes

12 themes available from the dropdown in the header: Catppuccin Mocha, Dracula, Nord, Solarized Dark, Solarized Light, One Dark, Gruvbox, Monokai Dimmed, Abyss, Catppuccin Latte, GitHub Light, and IBM 3278 Retro. Your selection is saved.

## Short Links

Every linx has a short URL at `/{name}`. For links, this redirects to the destination URL and tracks the click. For people, it shows their profile page with contact info and social links.

## Detail Pages

Append `+` to any short name to view its detail page instead of redirecting. For example, `/mylink+` shows the link's destination, owner, click count, and creation date. For people linx, it shows the profile page.

## Path Passthrough

Links automatically pass through extra path segments. For example, if `/github` points to `https://github.com`, then `/github/anthropics/claude` redirects to `https://github.com/anthropics/claude`. Query parameters are also forwarded.

Advanced users can use Go template syntax in destination URLs — click the **?** button next to the Destination URL field for details.

## Local Aliases

Destination URLs can be a short name instead of a full URL. This creates a local alias — the server follows the chain internally without an extra redirect.

| Destination | Meaning |
|-------------|---------|
| `docs` | Alias to the `docs` short name (chain-followed server-side) |
| `http://go2/sometag` | Link to another server on the tailnet (scheme required) |
| `https://example.com` | External URL |

**Important:** Use just the short name for local aliases, not `go/docs`. The `go/` prefix is the server hostname — inside GoLinx, everything is referenced by short name alone. For links to other servers on your tailnet, include the `http://` scheme so GoLinx knows it's a different host.

## Permissions

GoLinx enforces owner-based permissions:

| Situation | Edit | Delete | Context Menu |
|-----------|------|--------|-------------|
| You own the linx | Yes | Yes | Edit + Delete |
| Linx has no owner | Yes (claims it) | Yes | Edit + Delete |
| Someone else owns it | No | No | View (readonly) |
| **Localhost** (127.0.0.1) | **Yes** | **Yes** | **Edit + Delete** |

- **Ownership** is set automatically when you create a linx — your Tailscale login (or `local@hostname` in local mode) becomes the owner.
- **Unowned linx** (empty owner) can be claimed by anyone — editing an unowned linx sets you as the owner.
- **Owners** can clear the owner field to make a linx unowned, or change it to transfer ownership.
- **Non-owners** see a readonly "Linx Info" modal — same fields, but disabled with no Save button.
- **Double-click** on a linx opens Edit if you own it, or Linx Info if you don't.
- **Localhost auto-admin** — requests from 127.0.0.1 or ::1 always have full access, no toggle needed.
- **Admin mode** — users with the Tailscale admin grant can toggle admin mode via the header switch to bypass ownership checks.

Permissions are enforced server-side — the API returns 403 Forbidden for unauthorized actions.

## Avatars

Person linx support avatar images. Upload via the Edit modal — pick a file and the preview updates immediately. Maximum file size is 5 MB.

## Settings

Theme, view mode, and sort mode (including Charts) are automatically saved and restored on your next visit.
