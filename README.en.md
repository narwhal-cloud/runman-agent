# NarwhalCloud Agent — Installation Guide

[中文](README.md)

---

## Overview

NarwhalCloud Agent (`narwhal-agent`) is the host-side daemon that manages container/VM instances on your VPS. It connects your server to the NarwhalCloud platform and exposes a local web management panel.

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

```bash
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
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
| 3 | **Incus (LXC)** *(experimental)*      | System containers via Incus. Lightweight alternative to VMs.                            |

> Types 2 and 3 are experimental and may not be stable in production.

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
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
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
