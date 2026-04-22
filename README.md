# NarwhalCloud Agent — Installation Guide / 安装指南

---

- [English](#english)
- [中文](#中文)

---

## English

### Overview

NarwhalCloud Agent (`narwhal-agent`) is the host-side daemon that manages container/VM instances on your VPS. It connects your server to the NarwhalCloud platform and exposes a local web management panel.

### System Requirements

| Item         | Requirement                                   |
|--------------|-----------------------------------------------|
| OS           | **Debian 13 (Trixie)** — strongly recommended |
| Architecture | x86_64 (amd64) · aarch64 (arm64)              |
| Memory       | ≥ 1 GB RAM                                    |
| Disk         | ≥ 10 GB free                                  |

> **Why Debian 13?** The install script uses `apt`, `podman`, `systemd-zram-generator`, and other packages that are fully available on Debian 13. Using other distributions may cause unexpected failures.

### Step 1 — Reinstall the OS (Recommended)

To ensure a clean, consistent environment, reinstall the server to Debian 13 using the [DD reinstall script](https://github.com/bin456789/reinstall/tree/main) before running the agent installer.

> **Warning:** This operation **erases the entire disk**. It does not support OpenVZ or LXC virtual machines.

**Outside China:**
```bash
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh || wget -O reinstall.sh https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
bash reinstall.sh debian 13
```

**Inside China:**
```bash
curl -O https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh || wget -O reinstall.sh https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh
bash reinstall.sh debian 13
```

The script will reboot the server and install Debian 13 automatically. Once the reboot is complete, SSH back in as root.

> To cancel before the reboot takes effect, run `bash reinstall.sh reset`.

### Step 2 — Run the Installer

```bash
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

The installer is interactive. It will ask you:

1. **Language** — English or 中文
2. **Virtualization type** — see below
3. **Public IPv6 detection** — whether to detect and configure IPv6
4. **Data disk size** — (Podman only) e.g. `20G`, `50G`
5. **Install rfw firewall** — optional eBPF firewall

### Virtualization Types

| # | Type                                  | Description                                                                             |
|---|---------------------------------------|-----------------------------------------------------------------------------------------|
| 1 | **Podman** *(recommended)*            | OCI containers via Podman. Lightweight, no KVM needed. Uses XFS loop-mounted data disk. |
| 2 | **cloud-hypervisor** *(experimental)* | Full KVM virtual machines. Requires `/dev/kvm`. Downloads Debian/Alpine VM images.      |
| 3 | **Incus (LXC)** *(experimental)*      | System containers via Incus. Lightweight alternative to VMs.                            |

> Types 2 and 3 are experimental and may not be stable in production.

### IPv6 Support

The installer auto-detects your server's IPv6 configuration and selects the appropriate mode:

| Mode     | Condition             | Behavior                                                  |
|----------|-----------------------|-----------------------------------------------------------|
| `none`   | No public IPv6        | IPv4 only                                                 |
| `snat`   | Single `/128` address | Containers/VMs share host IPv6 via SNAT/masquerade        |
| `subnet` | Prefix ≤ `/127`       | Each container/VM gets an independent public IPv6 address |

You can override the mode by setting the environment variable before running the script:

```bash
IPV6_MODE=snat bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

### What Gets Installed

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

### Step 3 — Bind Your Token

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

### Updating the Agent

Run the same install command on an already-installed host — it automatically detects the existing installation and performs an in-place update (agent + netavark + rfw):

```bash
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

### Service Management

```bash
# Check agent status
systemctl status narwhal-agent

# View logs
journalctl -u narwhal-agent -f

# Restart agent
systemctl restart narwhal-agent

# Check rfw firewall status
systemctl status rfw
```

### Key Configuration Fields

`/opt/narwhal-agent/config.json`:

| Field              | Description                                                   |
|--------------------|---------------------------------------------------------------|
| `token`            | Host token (fill in after installation)                       |
| `web`              | Web panel listen address (default `:8792`)                    |
| `virt_type`        | `podman` / `cloudhv` / `incus`                                |
| `monitor_nic`      | NIC to monitor for traffic stats (leave empty to auto-detect) |
| `ipv6_mode`        | `none` / `snat` / `subnet`                                    |
| `max_port_forward` | Maximum port-forward rules per container (default `20`)       |

### Troubleshooting

**Agent fails to start**
```bash
journalctl -u narwhal-agent --no-pager -n 50
```

**Podman data disk not mounted**
```bash
mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
systemctl restart narwhal-agent
```

**KVM not available (cloud-hypervisor)**
Enable nested virtualization in your hypervisor, or switch to Podman mode.

**Package installation fails**
The script retries up to 3 times and clears dpkg locks automatically. If it still fails, run `apt-get update` manually and retry.

---

## 中文

### 概述

NarwhalCloud Agent（`narwhal-agent`）是运行在母鸡上的后台服务，负责管理容器/虚拟机实例，将您的服务器接入 NarwhalCloud 平台，并提供本地 Web 管理面板。

### 系统要求

| 项目   | 要求                               |
|------|----------------------------------|
| 操作系统 | **Debian 13 (Trixie)** — 强烈推荐    |
| 架构   | x86_64 (amd64) · aarch64 (arm64) |
| 内存   | ≥ 1 GB RAM                       |
| 磁盘   | ≥ 10 GB 可用空间                     |

> **为什么选 Debian 13？** 安装脚本使用了 `apt`、`podman`、`systemd-zram-generator` 等依赖，这些在 Debian 13 上完整可用。使用其他发行版可能导致意外失败。

### 第一步 — 重装系统（推荐）

为确保干净一致的运行环境，建议在运行 Agent 安装脚本前，先通过 [DD 重装脚本](https://github.com/bin456789/reinstall/tree/main) 将系统重装为 Debian 13。

> **警告：** 此操作会**清除整块硬盘的所有数据**。不支持 OpenVZ 或 LXC 虚拟机。

**境外服务器：**
```bash
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh || wget -O reinstall.sh https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
bash reinstall.sh debian 13
```

**境内服务器：**
```bash
curl -O https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh || wget -O reinstall.sh https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh
bash reinstall.sh debian 13
```

脚本将自动重启服务器并安装 Debian 13。重启完成后，重新以 root 身份 SSH 登入。

> 如需在重启生效前取消操作，执行 `bash reinstall.sh reset`。

### 第二步 — 执行安装脚本

```bash
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

安装脚本为交互式，过程中会依次询问：

1. **语言选择** — English 或 中文
2. **虚拟化类型** — 详见下方说明
3. **公网 IPv6 检测** — 是否检测并配置 IPv6
4. **数据盘大小** — 仅 Podman 模式需要，例如 `20G`、`50G`
5. **是否安装 rfw 防火墙** — 可选的 eBPF 防火墙

### 虚拟化类型说明

| 编号 | 类型                        | 说明                                                 |
|----|---------------------------|----------------------------------------------------|
| 1  | **Podman**（推荐）            | 基于 Podman 的 OCI 容器，轻量，无需 KVM。使用 XFS loop 挂载数据盘。    |
| 2  | **cloud-hypervisor**（实验性） | 完整 KVM 虚拟机，需要 `/dev/kvm`。自动下载 Debian/Alpine 虚拟机镜像。 |
| 3  | **Incus (LXC)**（实验性）      | 基于 Incus 的系统容器，比 VM 更轻量。                           |

> 选项 2 和 3 目前处于实验阶段，生产环境稳定性不保证。

### IPv6 支持

安装脚本会自动检测服务器的 IPv6 配置并选择合适的模式：

| 模式       | 触发条件         | 行为                                  |
|----------|--------------|-------------------------------------|
| `none`   | 无公网 IPv6     | 仅 IPv4                              |
| `snat`   | 单个 `/128` 地址 | 容器/VM 通过 SNAT/MASQUERADE 共享宿主机 IPv6 |
| `subnet` | 前缀 ≤ `/127`  | 每个容器/VM 获得独立的公网 IPv6 地址             |

也可通过环境变量强制指定模式：

```bash
IPV6_MODE=snat bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

### 安装内容

| 组件          | 路径                                       |
|-------------|------------------------------------------|
| Agent 二进制   | `/opt/narwhal-agent/narwhal-agent`       |
| 配置文件        | `/opt/narwhal-agent/config.json`         |
| Agent 数据库   | `/opt/narwhal-agent/agent.db`            |
| 数据目录        | `/var/lib/narwhal-agent`                 |
| Podman 数据盘  | `/xfs_disk.img` → 挂载至 `/data`            |
| Systemd 服务  | `narwhal-agent.service`                  |
| rfw 防火墙（可选） | `/opt/narwhal-agent/rfw` + `rfw.service` |

**Web 管理面板**：`http://<服务器IP>:8792`

### 第三步 — 绑定 Token

安装完成后，终端会显示服务器 IP 和面板地址：

```
[2026-01-01 00:00:00] ========================================
[2026-01-01 00:00:00] ✓ NarwhalCloud Agent 安装完成！
[2026-01-01 00:00:00] IP:           1.2.3.4
[2026-01-01 00:00:00] 面板地址:     http://1.2.3.4:8792
[2026-01-01 00:00:00] 下一步：登录面板并在设置中填入您的 Token
[2026-01-01 00:00:00] ========================================
```

1. 在浏览器中打开 `http://<服务器IP>:8792`
2. 登录管理面板
3. 进入**设置**页面，将 NarwhalCloud 控制台中的**母鸡 Token** 粘贴并保存

### 更新 Agent

在已安装的服务器上重新执行同一命令，脚本会自动检测到已有安装并执行就地更新（Agent + netavark + rfw 同步更新）：

```bash
bash <(curl -fsSL https://github.com/narwhal-cloud/runman-agent/releases/latest/download/install.sh)
```

### 服务管理

```bash
# 查看 Agent 状态
systemctl status narwhal-agent

# 实时查看日志
journalctl -u narwhal-agent -f

# 重启 Agent
systemctl restart narwhal-agent

# 查看 rfw 防火墙状态
systemctl status rfw
```

### 关键配置字段

`/opt/narwhal-agent/config.json`：

| 字段                 | 说明                             |
|--------------------|--------------------------------|
| `token`            | 母鸡 Token（安装完成后填入）              |
| `web`              | 面板监听地址（默认 `:8792`）             |
| `virt_type`        | `podman` / `cloudhv` / `incus` |
| `monitor_nic`      | 用于流量统计的网卡名（留空自动检测）             |
| `ipv6_mode`        | `none` / `snat` / `subnet`     |
| `max_port_forward` | 每个容器的最大端口转发规则数（默认 `20`）        |

### 常见问题排查

**Agent 启动失败**
```bash
journalctl -u narwhal-agent --no-pager -n 50
```

**Podman 数据盘未挂载**
```bash
mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
systemctl restart narwhal-agent
```

**KVM 不可用（cloud-hypervisor 模式）**
在宿主机管理界面开启嵌套虚拟化，或改用 Podman 模式。

**软件包安装失败**
脚本会自动重试 3 次并清理 dpkg 锁。若仍失败，手动执行 `apt-get update` 后再试。
