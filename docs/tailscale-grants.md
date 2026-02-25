# Setting Up Admin Access with Tailscale Grants

This guide walks you through configuring GoLinx admin users via Tailscale ACL grants. Admin users can toggle "Admin mode" in the GoLinx header to bypass ownership checks — editing or deleting any linx regardless of owner.

> **Prerequisite:** GoLinx is already running on your tailnet as a tsnet node (e.g. `go.your-tailnet.ts.net`) and basic connectivity is working.

---

## Overview

GoLinx reads admin status from **Tailscale grants**, not from a config file. When a user connects, GoLinx checks their Tailscale identity for the `tailscale.com/cap/golinx` capability with `"admin": true`. This is configured entirely in the Tailscale Admin Console — no GoLinx restart needed when you add or remove admins.

---

## Step 1: Create a Group for Admins

1. Open the [Tailscale Admin Console](https://login.tailscale.com/admin/acls/file)
2. In your ACL policy file, add a **group** for GoLinx admins:

```jsonc
{
  "groups": {
    "group:golinx-admins": ["alice@example.com", "bob@example.com"]
  }
}
```

Replace the emails with the Tailscale login names of users who should have admin access. You can find login names on the [Users page](https://login.tailscale.com/admin/users) in the Admin Console.

> **Tip:** You can skip the group and use individual users directly in the grant's `src` field, but groups make it easier to manage as your team changes.

---

## Step 2: Define the Tag Owner

GoLinx automatically advertises `tag:golinx` on your tailnet. For Tailscale to accept the tag, you need to define who owns it in your ACL policy file:

```jsonc
{
  "tagOwners": {
    "tag:golinx": ["your-login@example.com"]
  }
}
```

Replace `your-login@example.com` with the Tailscale login of the person who runs the GoLinx server. You can also use a group (e.g. `group:golinx-admins`) as the tag owner.

Once this is saved and GoLinx restarts, the node will appear as `tag:golinx` on the [Machines page](https://login.tailscale.com/admin/machines). The node may need to re-authenticate once.

**What tagging does:**

- Key expiry is **disabled** — no more 180-day reauth prompts (ideal for servers)
- The node is **owned by the tag** instead of a user — this is fine for GoLinx since it's a service
- The node **cannot initiate connections to shared nodes** — irrelevant for GoLinx since it only receives requests

> **Note:** If `tagOwners` isn't configured yet, GoLinx still works normally — it just runs untagged. You only need the tag when you're ready to set up grants.

---

## Step 3: Add the Grants

Add two grants to your ACL policy file:

```jsonc
{
  "grants": [
    {
      // Allow all tailnet members to access GoLinx
      "src": ["autogroup:member"],
      "dst": ["tag:golinx"],
      "ip": ["*"]
    },
    {
      // Give admin capability to the admin group
      "src": ["group:golinx-admins"],
      "dst": ["tag:golinx"],
      "app": {
        "tailscale.com/cap/golinx": [{ "admin": true }]
      }
    }
  ]
}
```

**What each grant does:**

| Grant | Purpose |
|-------|---------|
| First (`ip: ["*"]`) | Allows network access to GoLinx for all tailnet members. Without this, nobody can reach the node. |
| Second (`app: ...`) | Attaches the `admin` capability to requests from `group:golinx-admins`. GoLinx reads this to determine admin status. |

---

## Step 4: Verify

1. Open GoLinx in your browser (`https://go.your-tailnet.ts.net` or whatever your URL is)
2. If you're in the admin group, you should see the **Admin** toggle in the header
3. Toggle it on — you can now edit or delete any linx, regardless of owner
4. Users **not** in the admin group will not see the toggle

### Troubleshooting

**Admin toggle doesn't appear?**

- Verify your Tailscale login is in `group:golinx-admins` (check [Users page](https://login.tailscale.com/admin/users))
- Verify the GoLinx node is tagged as `tag:golinx` (check [Machines page](https://login.tailscale.com/admin/machines))
- Verify the grants are saved in the ACL policy (check for JSON syntax errors)
- Hard-refresh the browser (`Ctrl+Shift+R`) to re-fetch `/api/whoami`

**Localhost access?**

Localhost (127.0.0.1 / ::1) always has full admin access regardless of grants. This is useful for initial setup and debugging.

---

## Complete ACL Example

Here's a minimal but complete ACL policy file with GoLinx grants:

```jsonc
{
  "groups": {
    "group:golinx-admins": ["alice@github", "bob@github"]
  },

  "tagOwners": {
    "tag:golinx": ["group:golinx-admins"]
  },

  "acls": [
    // ... your existing ACL rules ...
  ],

  "grants": [
    {
      "src": ["autogroup:member"],
      "dst": ["tag:golinx"],
      "ip": ["*"]
    },
    {
      "src": ["group:golinx-admins"],
      "dst": ["tag:golinx"],
      "app": {
        "tailscale.com/cap/golinx": [{ "admin": true }]
      }
    }
  ]
}
```

---

## Adding or Removing Admins

To change who has admin access, edit the `group:golinx-admins` membership in your Tailscale ACL policy. Changes take effect immediately — no GoLinx restart needed.

```jsonc
"groups": {
  "group:golinx-admins": ["alice@github", "bob@github", "charlie@github"]
}
```
