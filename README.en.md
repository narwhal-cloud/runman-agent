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

With no arguments the installer opens a guided operations menu: install/update,
IPv6 detection, manual `/64` validation, Token display/rotation, network
backup/rollback, normal uninstall, and full Incus cleanup. The Incus install
wizard offers NAT4 + public IPv6, IPv6-only, manual routed/native `/64`, IPv4
only, and IPv6 SNAT scenarios.

```bash
# NAT4 + auto-detected public IPv6; podcctv Alpine 3.24 is the default image
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  en --virt incus --nat4 --non-interactive --generate-token

# IPv6-only with automatic native/tunnel routed-prefix detection
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  en --virt incus --ipv6-only --non-interactive

# Manual routed /64 (HE 6in4, WireGuard, provider static route)
bash install.sh en --virt incus --nat4 --non-interactive \
  --ipv6-mode subnet --ipv6-addr 2001:db8:100::1 \
  --ipv6-subnet 2001:db8:100::/64 --ipv6-iface wg6 --ipv6-routed

# Manual native on-link /64 (omit --ipv6-routed so NDP mode is used)
bash install.sh en --virt incus --nat4 --non-interactive \
  --ipv6-mode subnet --ipv6-addr 2001:db8:200::1 \
  --ipv6-subnet 2001:db8:200::/64 --ipv6-iface eth0

# Validate manual values and live source-address routing without installing
bash install.sh en --validate-ipv6 --ipv6-mode subnet \
  --ipv6-addr 2001:db8:100::1 --ipv6-subnet 2001:db8:100::/64 \
  --ipv6-iface wg6 --ipv6-routed
```

## Virtualization Types

| # | Type                                  | Description                                                                             |
|---|---------------------------------------|-----------------------------------------------------------------------------------------|
| 1 | **Podman** *(recommended)*            | OCI containers via Podman. Lightweight, no KVM needed. Uses XFS loop-mounted data disk. |
| 2 | **cloud-hypervisor** *(experimental)* | Full KVM virtual machines. Requires `/dev/kvm`. Downloads Debian/Alpine VM images.      |
| 3 | **Incus (LXC)** *(enhanced in this fork)* | System containers via Incus. Lightweight alternative to VMs. This fork adds image mirroring, banners, fine-grained IPv6, pure-IPv6, password-login fallback, backup/rollback, and one-click uninstall. |

> Type 2 is experimental. Type 3 (Incus) is production-ready in this fork.

## Fork Enhancements (podcctv)

This fork of `narwhal-cloud/runman-agent` significantly enhances the **Incus (LXC)** backend and offline/intranet deployment. Highlights:

- **Guided operations menu** — install/update, six network scenarios, IPv6 detection/validation, Token lifecycle, backup/rollback, and two uninstall levels.
- **Private image server and default custom image** — built-in `https://alpine-incus-base.428048.xyz`; the installer verifies simplestreams checksums and imports Alpine 3.24 as `alpine/3.24/cloud/<arch>/ready`. Alpine is first in the panel image list. Override with `--image-mirror` or `--local-image-dir`.
- **Custom alpine base** — `--alpine-base <local.tar.gz | incus-alias>`.
- **SSH login banner** — `--banner-preset none|default|minimal|project|custom` (+ `--banner-text`).
- **Fine-grained IPv6 allocation** — `--ipv6-alloc N` (N addresses per container).
- **Pure IPv6 containers** — `--ipv6-only` (no IPv4 assigned).
- **Forced SSH password login** — injected into every container regardless of base image.
- **IPv6 backup / rollback** — pre-change backup of sysctl/network/incusbr0; `--backup-ipv6` / `--rollback-ipv6`.
- **Token lifecycle** — supply/generate at install, `--show-token`, and generated or custom `--rotate-token` with an automatic Agent restart.
- **Two-level uninstall** — `--uninstall` preserves Incus artifacts; add `--purge-incus` for a clean reinstall that removes managed instances, ready images, `incusbr0`, and `podcctv-mirror`.

Detailed design and code review: [`INCU_S_OPTIMIZATION_REPORT.md`](INCU_S_OPTIMIZATION_REPORT.md).

## IPv6 Support

The installer auto-detects your server's IPv6 configuration and selects the appropriate mode:

| Mode     | Condition                             | Behavior                                                  |
|----------|---------------------------------------|-----------------------------------------------------------|
| `none`   | No public IPv6                        | IPv4 only                                                 |
| `snat`   | Single `/128`, or prefix `/65`–`/127` | Containers/VMs share host IPv6 via SNAT/masquerade        |
| `subnet` | Prefix ≤ `/64` (at least a `/64`)     | Each container/VM gets an independent public IPv6 address |

> **Subnet mode requires at least a `/64` block.** Prefixes `/65`–`/127` do not provide enough address space to allocate individual addresses to containers/VMs and automatically fall back to SNAT mode.

`--nat4` keeps an RFC1918 address on every instance with Incus IPv4 NAT while
also assigning public IPv6. `--ipv6-only` sets the instance NIC's
`ipv4.address=none`. Auto-detection distinguishes native on-link prefixes from
HE/WireGuard/provider-routed prefixes and verifies a temporary independent
source address before accepting the `/64`.

You can override the mode and network parameters by setting environment variables before running the script:

```bash
# Force SNAT mode
IPV6_MODE=snat bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)

# Force subnet mode with specific IP and subnet
IPV6_MODE=subnet IPV6_ADDR=2001:db8::1 IPV6_SUBNET=2001:db8::/64 IPV6_IFACE=eth0 \
  bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

Use `bash install.sh --detect-ipv6` for detection only. Use
`--skip-ipv6-probe` only for intentional offline validation or CI; normal
installation stops when live source-address verification fails.

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

## Step 3 — Integration Token and Rotation

Pass an existing platform Token with `--token`, or use `--generate-token` (or
leave the interactive prompt empty) to create a 48-character random Token.
Installation prints the panel credentials, Token, and rotation command once:

```
[2026-01-01 00:00:00] ========================================
[2026-01-01 00:00:00] ✓ NarwhalCloud Agent installation complete!
[2026-01-01 00:00:00] IP:           1.2.3.4
[2026-01-01 00:00:00] Web panel:    http://1.2.3.4:8792
[2026-01-01 00:00:00] Token:        <48-character-token>
[2026-01-01 00:00:00] Token rotation: bash install.sh --rotate-token
[2026-01-01 00:00:00] ========================================
```

```bash
bash install.sh --show-token
bash install.sh --rotate-token
bash install.sh --rotate-token --token 'custom-token-at-least-16-characters'

# Avoid placing a custom value in shell history
read -rsp 'Token: ' NARWHAL_AGENT_TOKEN; export NARWHAL_AGENT_TOKEN
bash install.sh --rotate-token
unset NARWHAL_AGENT_TOKEN
```

Synchronize the newly printed value with the integration platform after every
rotation. The web-panel password and integration Token are separate credentials.

## Clean Reinstall / Uninstall

```bash
# Restore network changes and remove Agent; keep Incus artifacts
bash install.sh --uninstall

# Also delete managed Incus instances, ready images, incusbr0 and podcctv-mirror
bash install.sh --uninstall --purge-incus
```

The full cleanup does not remove the Incus package/default storage pool or
touch HE/WireGuard tunnel services, so the guided installer can be run again.

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
| `token`            | Platform integration Token (created/supplied at install; view/rotate via installer) |
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
