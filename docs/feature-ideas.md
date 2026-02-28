# Redundant GoLinx with Tailscale Services

Two options for running redundant GoLinx instances with automatic failover via Tailscale Services. Both use the same Tailscale Services configuration for DNS and routing — the difference is in how the database stays in sync between nodes. This is way overkill for a link shortener service for anything but very large deployments, but is interesting to consider how to architect if you had to.

---

## Option 1: LiteFS (Automatic Failover)

Uses LiteFS (FUSE-based SQLite replication) for automatic leader election and data sync. When the primary dies, LiteFS promotes a replica and GoLinx re-registers the Tailscale Service automatically.

### Component Roles

| Component | Responsibility |
|-----------|---------------|
| **LiteFS** | Leader election and SQLite replication via FUSE mount. |
| **tsnet** | Embeds Tailscale into the Go binary so each instance is its own network node. |
| **Tailscale Service** | Provides a TailVIP and MagicDNS name (`go`) decoupled from hardware. |

### GoLinx Changes

Add a `--litefs-primary` flag pointing to the LiteFS primary sentinel file. When set, a background goroutine watches for leadership changes and registers/unregisters the Tailscale Service.

```go
var litefsPrimary = flag.String("litefs-primary", "", "path to LiteFS .primary file (enables leader-watch loop)")

// watchLeadership monitors LiteFS leader status and updates Tailscale Service registration.
func watchLeadership(ctx context.Context, tsSrv *tsnet.Server, primaryPath string) {
    isLeader := false

    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        _, err := os.Stat(primaryPath)
        currentlyLeader := (err == nil)

        if currentlyLeader && !isLeader {
            log.Printf("Promoted to leader — registering Tailscale Service svc:go")
            tsSrv.RegisterService("svc:go", 80)
            isLeader = true
        } else if !currentlyLeader && isLeader {
            log.Printf("Demoted — unregistering Tailscale Service svc:go")
            tsSrv.UnregisterService("svc:go", 80)
            isLeader = false
        }

        time.Sleep(5 * time.Second)
    }
}
```

Launch the goroutine after `initTailscale()`:

```go
if *litefsPrimary != "" {
    go watchLeadership(ctx, tsSrv, *litefsPrimary)
}
```

### Operational Flow

**Normal operation:**

1. Client connects to `go/jira` (the Tailscale Service MagicDNS name).
2. Tailscale routes to whichever node currently has `RegisterService` active.
3. GoLinx reads/writes the SQLite file mounted by LiteFS.

**Failover:**

1. Node A (primary) crashes or loses its LiteFS lease.
2. LiteFS promotes Node B — creates the `.primary` sentinel file.
3. Node B's `watchLeadership` loop detects the file and calls `RegisterService`.
4. Tailscale updates routing — all traffic to `go` now reaches Node B.
5. Fully automatic, no manual intervention.

### Trade-offs

- **Pro:** Fully automatic failover, no manual promotion step.
- **Con:** Requires FUSE (Linux only), adds LiteFS as an operational dependency, LiteFS development has slowed since Ben Johnson left Fly.io.

---

## Option 2: Litestream + Write Forwarding (Simple Failover)

Uses Litestream (WAL streaming to S3) for data sync and application-level write forwarding for read/write separation. Simpler than LiteFS — no FUSE, no leader election — but requires a short manual promotion step on failover.

### Component Roles

| Component | Responsibility |
|-----------|---------------|
| **Litestream** | Streams SQLite WAL to S3 (primary) or restores from S3 (replica). |
| **tsnet** | Embeds Tailscale into the Go binary so each instance is its own network node. |
| **Tailscale Service** | Provides a TailVIP and MagicDNS name (`go`) decoupled from hardware. |

### GoLinx Changes

**New flag:** `--primary-url`

```go
var primaryURL = flag.String("primary-url", "", "URL of the primary instance (enables read-replica mode)")
```

**Write forwarding:** When `--primary-url` is set, mutation handlers proxy to the primary instead of writing locally. Add a helper early in `Run()`:

```go
var writeProxy *httputil.ReverseProxy
if *primaryURL != "" {
    u, err := url.Parse(*primaryURL)
    if err != nil {
        return fmt.Errorf("invalid --primary-url: %w", err)
    }
    writeProxy = httputil.NewSingleHostReverseProxy(u)
}
```

Then in each mutation handler (`apiLinxCreate`, `apiLinxUpdate`, `apiLinxDelete`, `apiLinxRestore`):

```go
func apiLinxCreate(w http.ResponseWriter, r *http.Request) {
    if writeProxy != nil {
        writeProxy.ServeHTTP(w, r)
        return
    }
    // ... existing logic ...
}
```

**Tailscale Service registration:** Both primary and replica register with the same service. Both serve reads (redirects). Only the primary accepts writes.

```go
// After initTailscale(), both nodes register as hosts for svc:go.
tsSrv.RegisterService("svc:go", 80)
```

### Operational Flow

**Normal operation:**

```
  go/jira ──► Tailscale Service (svc:go)
                 ├──► Node A (primary) ── read/write, Litestream push → S3
                 └──► Node B (replica) ── read-only, Litestream restore ← S3
                                          writes proxied → Node A
```

1. Client hits `go/jira` — Tailscale routes to either node.
2. Redirects (reads) are served locally on both nodes — fast, no round-trip.
3. Edits (writes) on the replica are proxied to the primary via `writeProxy`.
4. Litestream streams changes from primary → S3 → replica (~1-2s lag).

**Failover (manual, ~30 seconds):**

1. Node A (primary) dies — Tailscale stops routing to it automatically.
2. All traffic now hits Node B (already serving reads).
3. Promote Node B:

```bash
# Stop restoring, start pushing
systemctl stop litestream-restore
systemctl start litestream-push

# Restart GoLinx without --primary-url
systemctl restart golinx
```

4. Node B is now the primary — reads and writes both served locally.

This can be scripted into a single `promote.sh` for one-command failover.

### Trade-offs

- **Pro:** No FUSE, no leader election, minimal dependencies (Litestream is a single static binary). Write forwarding is a small code change (~10 lines per handler).
- **Con:** Failover requires a manual promotion step (or a cron/health-check script to automate it). ~1-2 second replication lag on reads at the replica.

---

## Tailscale Services: Admin Console Setup

Both options use the same Tailscale configuration:

### 1. Create the Service

In the Tailscale admin console → **Services** tab:

- **Name:** `go`
- **Endpoints:** `tcp:80` (add `tcp:443` if using HTTPS)

This creates the TailVIP and MagicDNS name `go.tailnet.ts.net`.

### 2. Create an Auth Key

Go to **Settings → Keys → Generate auth key**:

- **Tag:** `tag:golinx`
- **Reusable:** Yes (both nodes use the same key)
- **Ephemeral:** Optional (nodes auto-cleanup if they go offline)

### 3. ACL Grants

Add to your Tailscale ACL policy to allow the tagged nodes to claim the service:

```json
{
  "grants": [
    {
      "src": ["tag:golinx"],
      "dst": ["svc:go"],
      "app": {
        "tailscale.com/cap/service-host": [{}]
      }
    }
  ]
}
```

### 4. GoLinx Configuration

Each node's `golinx.toml`:

**Node A (primary):**

```toml
listen = ["ts+http://:80"]
ts-hostname = "go-node-a"
```

**Node B (replica — Option 2 only):**

```toml
listen = ["ts+http://:80"]
ts-hostname = "go-node-b"
primary-url = "http://go-node-a:80"
```

### 5. Approve Hosts

When each node first starts and joins the tailnet, go to **Services → go → Hosts** and approve it (or configure auto-approval in ACLs).

Both nodes now advertise themselves as hosts for `svc:go`. Tailscale routes clients to the nearest available host. If one goes down, traffic automatically shifts to the other.

---

## Comparison

| | Option 1: LiteFS | Option 2: Litestream |
|---|---|---|
| **Replication** | FUSE mount, automatic | WAL streaming via S3 |
| **Failover** | Automatic (leader election) | Manual promotion (~30s) |
| **Dependencies** | LiteFS (FUSE, Linux only) | Litestream + S3 bucket |
| **Code changes** | Leader-watch goroutine | Write-forwarding proxy |
| **Complexity** | Higher | Lower |
| **Replication lag** | Near-zero | ~1-2 seconds |
| **Recommended for** | Multi-node clusters needing zero-downtime | Two-node redundancy with simple ops |
