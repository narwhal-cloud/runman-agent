# NarwhalCloud Agent — Installation Guide

> **Turn your idle server into a NAT VPS shared-hosting node. Amortize your costs. Earn revenue per day, per tenant.**

[中文](README.md) | [Dashboard](https://dash.fuckip.me) | [Website](https://fuckip.me) | **[Fork (podcctv)](https://github.com/podcctv/runman-agent)**

---

## What Is This?

**NarwhalCloud** is a C2C NAT VPS sharing marketplace. You connect your server — a dedicated box, a high-bandwidth VPS, or a co-located machine — to the platform. The platform slices your server's resources into individual **NAT VPS instances** and sells them to tenants on a per-day basis. Revenue is split 80/20 in your favor. Your hosting and bandwidth bills get amortized across every paying tenant.

```
Your server ──► NarwhalCloud Agent ──► NAT VPS instances (tenants pay per day)
                                              │
                   Platform keeps 20% ◄──────┤
                   You earn 80%       ◄──────┘
```

**The cost math is straightforward.** A $50/month dedicated server running 30 NAT VPS instances at $3/month each generates $90/month — your costs are covered and you net $40, plus free compute for your own use.

### What Is a NAT VPS?

A NAT VPS is a virtual machine that shares a single public IP with other VMs on the same host and exposes services through port forwarding rules. Because it eliminates the cost of a dedicated IPv4 address, a NAT VPS typically sells for 1/5 to 1/3 the price of an equivalent dedicated-IP VPS.

For the majority of use cases — scrapers, bots, reverse proxies, learning Linux, self-hosting behind Cloudflare, game servers — a NAT VPS works exactly as well as a dedicated-IP VPS. NarwhalCloud Agent is the daemon that gives your server the ability to carve out NAT VPS instances, manage port forwarding, account for traffic, and connect to the platform — automatically.

### What Servers Can You Connect?

| Server Type | Typical Scenario |
|---|---|
| Dedicated server / bare metal | Core use case — high density, highest revenue |
| High-bandwidth or multi-IP VPS | Monetize idle bandwidth and ports |
| Co-located server | Offset rack rent and transit with tenant revenue |
| Home-lab server | Cover electricity bills with lightweight shared hosting |

---

## Overview (Technical)

NarwhalCloud Agent (`narwhal-agent`) is the host-side daemon that manages container/VM instances on your server. It connects your machine to the NarwhalCloud platform, exposes a local web management panel, and handles NAT port forwarding, traffic metering, and IPv6 allocation automatically. The agent bridges the platform control plane and the tenant instances running on your hardware — it is the only process you need to install.

## System Requirements

| Item         | Requirement                                   |
|--------------|-----------------------------------------------|
| OS           | **Debian 13 (Trixie)** — strongly recommended |
| Architecture | x86_64 (amd64) · aarch64 (arm64)              |
| Memory       | ≥ 1 GB RAM                                    |
| Disk         | ≥ 10 GB free                                  |

> **Why Debian 13?** The install script uses `apt`, `podman`, `systemd-zram-generator`, and other packages that are fully available on Debian 13. Using other distributions may cause unexpected failures.

## Step 1 — Reinstall the OS (Recommended)

To ensure a clean, consistent environment, reinstall the server to Debian 13 using the [DD reinstall script](https://github.com/bin456789/reinstall/tree/main) before running the agent installer.

> **Warning:** This operation **erases the entire disk**. It does not support OpenVZ or LXC virtual machines.

```bash
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh || wget -O reinstall.sh https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
bash reinstall.sh debian 13
```

The script will reboot the server and install Debian 13 automatically. Once the reboot is complete, SSH back in as root.

> To cancel before the reboot takes effect, run `bash reinstall.sh reset`.

## Step 2 — Run the Installer

> This fork pulls the installer directly from the `main` branch (the fork does not publish releases):

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

The installer is interactive. It will ask you:

1. **Language** — English or 中文
2. **Virtualization type** — see below
3. **Public IPv6 detection** — whether to detect and configure IPv6
4. **Data disk size** — (Podman only) e.g. `20G`, `50G`
5. **Install rfw firewall** — optional eBPF firewall

## Virtualization Types

| # | Type                                  | Description                                                                             |
|---|---------------------------------------|-----------------------------------------------------------------------------------------|
| 1 | **Podman** *(recommended)*            | OCI containers via Podman. Lightweight, no KVM needed. Uses XFS loop-mounted data disk. |
| 2 | **cloud-hypervisor** *(experimental)* | Full KVM virtual machines. Requires `/dev/kvm`. Downloads Debian/Alpine VM images.      |
| 3 | **Incus (LXC)** *(enhanced in this fork)* | System containers via Incus. Lightweight alternative to VMs. This fork adds image mirroring, banners, fine-grained IPv6, pure-IPv6, password-login fallback, backup/rollback, and one-click uninstall. |

> Type 2 is experimental. Type 3 (Incus) is production-ready in this fork.

## Fork Enhancements (podcctv)

This fork of `narwhal-cloud/runman-agent` significantly enhances the **Incus (LXC)** backend and offline/intranet deployment. Highlights:

- **Private image server** — built-in default `https://alpine-incus-base.428048.xyz` (a LXD/Incus **simplestreams** server, not a flat tarball host); override with `--image-mirror <url>` or use a fully offline local directory via `--local-image-dir <dir>`. At install time the script registers it as `incus remote add podcctv-mirror <url> --protocol=simplestreams --public` (best-effort, also validates that `streams/v1/index.json` is present); the agent's runtime builder prefers this mirror for alpine and falls back to `images.linuxcontainers.org` on failure. The server's `index.json` is generated by `podcctv/alpine-base`'s `scripts/generate-streams.py` (make sure it produces a valid `datatype: index:1.0` simplestreams index).
- **Custom alpine base** — `--alpine-base <local.tar.gz | incus-alias>`.
- **SSH login banner** — `--banner-preset none|default|minimal|project|custom` (+ `--banner-text`).
- **Fine-grained IPv6 allocation** — `--ipv6-alloc N` (N addresses per container).
- **Pure IPv6 containers** — `--ipv6-only` (no IPv4 assigned).
- **Forced SSH password login** — injected into every container regardless of base image.
- **IPv6 backup / rollback** — pre-change backup of sysctl/network/incusbr0; `--backup-ipv6` / `--rollback-ipv6`.
- **One-click uninstall** — `--uninstall` restores IPv6/network first, then removes only agent-managed changes (incus containers/images and other services are preserved).

Detailed design and code review: [`INCU_S_OPTIMIZATION_REPORT.md`](INCU_S_OPTIMIZATION_REPORT.md).

## IPv6 Support

The installer auto-detects your server's IPv6 configuration and selects the appropriate mode:

| Mode     | Condition                             | Behavior                                                  |
|----------|---------------------------------------|-----------------------------------------------------------|
| `none`   | No public IPv6                        | IPv4 only                                                 |
| `snat`   | Single `/128`, or prefix `/65`–`/127` | Containers/VMs share host IPv6 via SNAT/masquerade        |
| `subnet` | Prefix ≤ `/64` (at least a `/64`)     | Each container/VM gets an independent public IPv6 address |

> **Subnet mode requires at least a `/64` block.** Prefixes `/65`–`/127` do not provide enough address space to allocate individual addresses to containers/VMs and automatically fall back to SNAT mode.

You can override the mode and network parameters by setting environment variables before running the script:

```bash
# Force SNAT mode
IPV6_MODE=snat bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)

# Force subnet mode with specific IP and subnet
IPV6_MODE=subnet IPV6_ADDR=2001:db8::1 IPV6_SUBNET=2001:db8::/64 bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

## What Gets Installed

| Component               | Path                                     |
|-------------------------|------------------------------------------|
| Agent binary            | `/opt/narwhal-agent/narwhal-agent`       |
| Config file             | `/opt/narwhal-agent/config.json`         |
| Agent database          | `/opt/narwhal-agent/agent.db`            |
| Data directory          | `/var/lib/narwhal-agent`                 |
| Podman data disk        | `/xfs_disk.img` → mounted at `/data`     |
| Systemd service         | `narwhal-agent.service`                  |
| rfw firewall (optional) | `/opt/narwhal-agent/rfw` + `rfw.service` |

**Web panel**: `http://<server-ip>:8792`

## Step 3 — Bind Your Token

After installation completes, the terminal displays your server's IP and the web panel URL:

```
[2026-01-01 00:00:00] ========================================
[2026-01-01 00:00:00] ✓ NarwhalCloud Agent installation complete!
[2026-01-01 00:00:00] IP:           1.2.3.4
[2026-01-01 00:00:00] Web panel:    http://1.2.3.4:8792
[2026-01-01 00:00:00] Next step: log in to the web panel and enter your Token
[2026-01-01 00:00:00] ========================================
```

1. Open `http://<server-ip>:8792` in your browser
2. Log in to the management panel
3. Navigate to **Settings** and paste your **Host Token** from the NarwhalCloud dashboard

## Updating the Agent

Run the same install command on an already-installed host — it automatically detects the existing installation and performs an in-place update (agent + netavark + rfw):

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

## Service Management

```bash
# Check agent status
systemctl status narwhal-agent

# View logs
journalctl -u narwhal-agent -f

# Restart agent
systemctl restart narwhal-agent

# Check rfw firewall status
systemctl status rfw

# Reset web panel password (requires restart)
/opt/narwhal-agent/narwhal-agent -reset-password NEW_PASSWORD
systemctl restart narwhal-agent
```

## Key Configuration Fields

`/opt/narwhal-agent/config.json`:

| Field              | Description                                                   |
|--------------------|---------------------------------------------------------------|
| `token`            | Host token (fill in after installation)                       |
| `web`              | Web panel listen address (default `:8792`)                    |
| `virt_type`        | `podman` / `cloudhv` / `incus`                                |
| `monitor_nic`      | NIC to monitor for traffic stats (leave empty to auto-detect) |
| `ipv6_mode`        | `none` / `snat` / `subnet`                                    |
| `max_port_forward` | Maximum port-forward rules per container (default `20`)       |
| `incus_image_mirror` | Private/local image base URL overriding GitHub Releases, e.g. `https://alpine-incus-base.428048.xyz` |
| `incus_alpine_base`  | Custom alpine base image: local tar.gz path or an existing incus alias |
| `incus_ipv6_alloc`  | Number of IPv6 addresses allocated per container (default `1`) |
| `incus_ipv6_only`   | `true` → new containers are pure-IPv6 (no IPv4)               |
| `incus_banner_preset` | Login banner preset: `none`/`default`/`minimal`/`project`/`custom` |
| `incus_banner_text` | Banner text when `preset=custom`                              |

## Troubleshooting

**Agent fails to start**
```bash
journalctl -u narwhal-agent --no-pager -n 50
```

**rfw fails to start**
Some cloud providers' network interface cards do not support eBPF. Try adding the `--xdp_mode skb` flag to the rfw service startup parameters.

**Podman data disk not mounted**
```bash
mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
systemctl restart narwhal-agent
```

**KVM not available (cloud-hypervisor)**
Enable nested virtualization in your hypervisor, or switch to Podman mode.

**Package installation fails**
The script retries up to 3 times and clears dpkg locks automatically. If it still fails, run `apt-get update` manually and retry.
