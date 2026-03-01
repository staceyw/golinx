# Tailscale Services History

The Tailscale Services tab in the admin console is much newer (launched late 2025) than the original `tailscale serve` and `tsnet` features.

The confusion usually comes because Tailscale recently merged these concepts. Here is the timeline of how they evolved:

## 1. The "Serve" Era (2023)

Originally, `tailscale serve` was just a CLI tool used to share a local port from your machine to your tailnet.

- **How it worked:** It didn't have its own tab in the admin console.
- **Hostname:** It was always tied to your machine's name (e.g., `my-laptop.tailnet.ts.net`).
- **Redundancy:** There was no way to have two machines share the same name.

## 2. The tsnet Era (Early 2023 onwards)

`tsnet` was released so developers could embed Tailscale into Go apps.

- **Goal:** Allow an app to join the network as its own "virtual machine" with its own IP and DNS name.
- **Limitation:** Like serve, each tsnet app was traditionally its own unique node. If you ran three instances, you got three different names.

## 3. The "Tailscale Services" Era (Late 2025 – Present)

This is what you see in the admin console now. It was created to solve the "Redundancy" problem by introducing **TailVIPs** (Virtual IPs).

- **The Hub:** The Services tab is now the central place to define a "Logical Identity" (like `svc:go`) that isn't tied to any one piece of hardware.
- **The Bridge:** Tailscale updated both `tailscale serve` and `tsnet` to work with this tab.
  - You can now run `tailscale serve --service=svc:go` to "link" a standard machine to that virtual identity.
  - You can use `tsnet` to register a service to that same identity.

## Summary

Before this new feature, there was no "Services" tab. Everything was just a "Machine." Now, the admin console allows you to create a Service first, and then use either `serve` (for existing apps) or `tsnet` (for custom code) to "claim" that service identity.
