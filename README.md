# NarwhalCloud Agent — 安装指南（podcctv 增强 Fork）

> **把你的闲置服务器变成 NAT VPS 共享主机，平摊成本，按天分成赚钱。**

[English](README.en.md) | [控制台](https://dash.fuckip.me) | [官网](https://fuckip.me) | **[本仓库 Fork](https://github.com/podcctv/runman-agent)**

---

> **📌 关于本 Fork**
>
> 本仓库是 [`narwhal-cloud/runman-agent`](https://github.com/narwhal-cloud/runman-agent) 的 **podcctv 增强分支**，在保留上游 NAT VPS 核心能力的基础上，重点强化了 **Incus (LXC) 后端**与**离线/内网部署能力**：
>
> - 🖥️ **私有镜像服务器**支持（默认内置 `https://alpine-incus-base.428048.xyz`，摆脱对 GitHub Releases 的依赖）
> - 📦 本地镜像目录离线导入、定制 `alpine-base` 基础镜像
> - 🪧 容器 SSH 登录欢迎页 / 横幅（预设 + 自定义）
> - 🌐 IPv6 精细化分配（每个容器可分配 N 个公网 IPv6）、纯 IPv6 容器
> - 🔐 强制容器内 SSH 用户密码登录（兼容定制镜像）
> - 💾 IPv6 / 网卡配置**变更前完整备份** + 一键回滚
> - 🗑️ 一键安装 / **一键卸载**（不影响既有业务、保留 incus 容器与镜像）
>
> 详细的设计与代码评审见 [`INCU_S_OPTIMIZATION_REPORT.md`](INCU_S_OPTIMIZATION_REPORT.md)。

---

## 这是什么？

**NarwhalCloud** 是一个 C2C NAT VPS 共享平台。你把自己的服务器（独立服务器、大带宽 VPS、共置托管机器）接入平台，平台负责将服务器的资源切分成多个 **NAT VPS 实例**，向平台上的租户按天出售。租户付的钱平台收取 20%，你拿 80%——你什么都不用做，服务器的托管费、带宽费自然就被平摊掉了。

```
你的服务器 ──► NarwhalCloud Agent ──► NAT VPS 实例（租户购买）
                                         │
                    平台收取 20% 佣金 ◄──┤
                    你获得 80% 收益  ◄──┘
```

**平摊成本的逻辑很简单：** 一台月费 $50 的独立服务器，跑 30 个 NAT VPS 实例，每个以 $3/月出售，月收入 $90，净赚 $40，还附带一批自用实例。成本不但清零，还有结余。

### 什么是 NAT VPS？

NAT VPS 是多台虚拟机共享同一个公网 IP、通过端口转发对外暴露服务的轻量 VPS 形态。因为省去了独立 IPv4 的成本，NAT VPS 的售价通常是独立 IP VPS 的 1/5 到 1/3——而对于爬虫、代理、机器人、学习 Linux、建站（配合 Cloudflare）等大多数使用场景来说，NAT VPS 完全够用。

NarwhalCloud Agent 就是让你的服务器具备"自动切分 NAT VPS 实例 + 管理端口转发 + 流量计费 + 接入平台"这套能力的守护进程。

### 适合接入的机器类型

| 机器类型 | 典型场景 |
|---|---|
| 独立服务器 / 裸金属 | 核心用法，资源多、密度大，收益最高 |
| 大带宽 / 多 IP VPS | 利用闲置带宽和端口对外出租 |
| 共置托管服务器 | 用出租收益抵消机柜租金和带宽费 |
| 家庭宽带服务器 | 接入平台实现轻量共享，收益覆盖电费 |

---

## 概述（技术）

NarwhalCloud Agent（`narwhal-agent`）是运行在宿主机（母鸡）上的后台服务，负责管理容器 / 虚拟机实例，将您的服务器接入 NarwhalCloud 平台，并提供本地 Web 管理面板。Agent 支持三种虚拟化后端（Podman、cloud-hypervisor KVM、Incus LXC），自动处理 NAT 端口转发、流量统计和 IPv6 分配，是平台侧与租户实例之间的唯一桥梁。

> **关于 Incus 后端**：上游将 Incus 标记为"实验性"，本 Fork 已对其做了大量生产化增强（私有镜像、欢迎页、IPv6 精细化/纯 IPv6、密码登录兜底、备份回滚、一键卸载），可放心用于 Incus 部署。

## 系统要求

| 项目   | 要求                               |
|------|----------------------------------|
| 操作系统 | **Debian 13 (Trixie)** — 强烈推荐    |
| 架构   | x86_64 (amd64) · aarch64 (arm64) |
| 内存   | ≥ 1 GB RAM                       |
| 磁盘   | ≥ 10 GB 可用空间                     |
| 后端依赖 | Incus 模式需提前安装 `incus` / `incus-ui` |

> **为什么选 Debian 13？** 安装脚本使用了 `apt`、`podman`、`systemd-zram-generator` 等依赖，这些在 Debian 13 上完整可用。使用其他发行版可能导致意外失败。

## 第一步 — 重装系统（推荐）

为确保干净一致的运行环境，建议在运行 Agent 安装脚本前，先通过 [DD 重装脚本](https://github.com/bin456789/reinstall/tree/main) 将系统重装为 Debian 13。

> **警告：** 此操作会**清除整块硬盘的所有数据**。不支持 OpenVZ 或 LXC 虚拟机。

```bash
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh || wget -O reinstall.sh https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
bash reinstall.sh debian 13
```

脚本将自动重启服务器并安装 Debian 13。重启完成后，重新以 root 身份 SSH 登入。

> 如需在重启生效前取消操作，执行 `bash reinstall.sh reset`。

## 第二步 — 执行安装脚本

> 本 Fork 直接从仓库 `main` 分支拉取安装脚本（fork 未发布 release）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

安装脚本为交互式，过程中会依次询问：

1. **语言选择** — English 或 中文
2. **虚拟化类型** — 详见下方说明
3. **公网 IPv6 检测** — 是否检测并配置 IPv6
4. **数据盘大小** — 仅 Podman 模式需要，例如 `20G`、`50G`
5. **是否安装 rfw 防火墙** — 可选的 eBPF 防火墙

### 非交互 / 一键参数

所有配置项均支持环境变量或命令行参数，便于自动化部署。常用示例：

```bash
# 使用私有镜像服务器（推荐，避免依赖 GitHub Releases）
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --image-mirror https://alpine-incus-base.428048.xyz

# 强制使用 Incus 后端 + 子网模式 IPv6 + 纯 IPv6 容器
IPV6_MODE=subnet IPV6_ADDR=2001:db8::1 IPV6_SUBNET=2001:db8::/64 \
  bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --virt incus --ipv6-only

# 使用定制 alpine 基础镜像 + SSH 欢迎页
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --alpine-base ./alpine-base.tar.gz --banner-preset project
```

## 虚拟化类型说明

| 编号 | 类型                        | 说明                                                 |
|----|---------------------------|----------------------------------------------------|
| 1  | **Podman**（推荐）            | 基于 Podman 的 OCI 容器，轻量，无需 KVM。使用 XFS loop 挂载数据盘。    |
| 2  | **cloud-hypervisor**（实验性） | 完整 KVM 虚拟机，需要 `/dev/kvm`。自动下载 Debian/Alpine 虚拟机镜像。 |
| 3  | **Incus (LXC)**（本 Fork 已增强） | 基于 Incus 的系统容器，比 VM 更轻量。本 Fork 为其新增镜像/欢迎页/IPv6/卸载等能力。 |

> 选项 2 和 3 上游处于实验阶段；**选项 3（Incus）在本 Fork 已具备生产可用性**。

---

## 本 Fork 的增强功能

| 功能 | 说明 | 相关参数 |
|---|---|---|
| 私有 / 本地镜像服务 | 用私有镜像服务器或本地目录替代 GitHub Releases 拉取镜像，离线/内网可部署 | `--image-mirror`、`--local-image-dir` |
| 定制 alpine 基础镜像 | 使用自己的 alpine-base（本地 tar.gz 或已导入的 incus 别名） | `--alpine-base` |
| SSH 欢迎页 / 横幅 | 容器登录前/后展示自定义横幅（预设或完全自定义文本） | `--banner-preset`、`--banner-text` |
| IPv6 精细化分配 | 每个容器可分配 N 个公网 IPv6（非 /64 网段也可） | `--ipv6-alloc` |
| 纯 IPv6 容器 | 容器仅分配 IPv6、不分配 IPv4 | `--ipv6-only` |
| 强制 SSH 密码登录 | 无论基础镜像如何，均确保容器内 root 密码登录可用 | （默认开启） |
| IPv6 备份 / 回滚 | 改动前完整备份 sysctl/网卡/incusbr0，支持一键回滚 | `--backup-ipv6`、`--rollback-ipv6` |
| 一键卸载 | 撤销本安装器引入的变更并先恢复 IPv6/网卡，不影响既有业务 | `--uninstall` |

下文针对 **Incus 后端**给出完整用法；Podman / cloud-hypervisor 模式下镜像镜像服务（`--image-mirror` 等）同样适用。

---

## IPv6 支持

安装脚本会自动检测服务器的 IPv6 配置并选择合适的模式：

| 模式       | 触发条件                          | 行为                                  |
|----------|-------------------------------|-------------------------------------|
| `none`   | 无公网 IPv6                      | 仅 IPv4                              |
| `snat`   | 单个 `/128` 地址，或前缀 `/65`–`/127` | 容器/VM 通过 SNAT/MASQUERADE 共享宿主机 IPv6 |
| `subnet` | 前缀 ≤ `/64`（至少 `/64` 子网）       | 每个容器/VM 获得独立的公网 IPv6 地址             |

> **子网模式要求至少分配到 `/64` 段。** 前缀 `/65`–`/127` 地址空间不足以为每个容器/VM 分配独立地址，会自动回退到 SNAT 模式。

也可通过环境变量强制指定模式及网络参数：

```bash
# 强制使用 SNAT 模式
IPV6_MODE=snat bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)

# 强制使用子网模式并指定 IP 和子网
IPV6_MODE=subnet IPV6_ADDR=2001:db8::1 IPV6_SUBNET=2001:db8::/64 \
  bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

---

## Incus 后端专属配置与用法

以下所有能力均作用于 **`virt_type=incus`**。既可在安装时通过参数指定，也可写入 `config.json` 后由 Agent 读取。

### 1. 私有镜像服务器（fork 默认源，推荐）

本 Fork 默认内置镜像源为 **`https://alpine-incus-base.428048.xyz`**，是一个 **LXD/Incus simplestreams 镜像服务器**（原生格式 `lxd.tar.xz` + `rootfs.squashfs`），而非扁平 tarball 服务。安装脚本会自动：

1. 读取 `https://alpine-incus-base.428048.xyz/streams/v1/images.json`；
2. 按其中声明的路径下载对应发行版的 `lxd.tar.xz` 与 `rootfs.squashfs`；
3. 执行 `incus image import` 并自动别名化为 Agent 期望的 `alpine/3.23/cloud/amd64/ready`。

> 该服务器当前发布 **Alpine（amd64）**。Debian 等未发布的发行版会自动从 GitHub Releases 默认源补齐；因此纯离线部署请配合 `--local-image-dir` 预置全部发行版镜像。

```bash
# fork 默认即使用私有镜像服务器，以下显式写法等价
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --image-mirror https://alpine-incus-base.428048.xyz

# 用你自己的 simplestreams 镜像服务器（任意实现了 streams/v1/images.json 的服务）
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --image-mirror https://your-mirror.example.com

# 完全离线：本地目录预置 incus-<distro>-<arch>.tar.gz（无需任何网络）
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --local-image-dir /root/incus-images
```

> **兼容性**：`--image-mirror` 同时支持最简流（simplestreams）与传统的扁平 tarball 基址——脚本会先尝试 simplestreams 元数据，失败则回退到 `<mirror>/incus-<distro>-<arch>.tar.gz`。留空（或 `--image-mirror ""`）则直接使用 GitHub Releases。

> **simplestreams 端点要求 `index.json`**：要让 `incus remote add --protocol=simplestreams` 或 Agent 运行时按 simplestreams 协议拉取，服务器根下必须存在合法的 `streams/v1/index.json`（格式为 `datatype: index:1.0` / `format: simplestreams:1.0`，`index` 指针指向 `streams/v1/images.json`）。本项目配套的镜像服务器构建脚本 `podcctv/alpine-base` 的 `scripts/generate-streams.py` 已修正此前误把 image-downloads 内容写入 `index.json` 的 bug；若你的服务器返回 404 或 `incus remote add` 失败，请重新运行 `python3 scripts/generate-streams.py` 并重新部署（或直接把一份正确的 `index.json` 放到 `streams/v1/` 下）。

#### 注册为 incus remote（运维便利 + 运行时验证）

安装脚本会在导入镜像后 **best-effort** 执行：

```bash
incus remote add podcctv-mirror https://alpine-incus-base.428048.xyz \
  --protocol=simplestreams --public
```

注册成功后即可直接：

```bash
incus launch podcctv-mirror:alpine/3.23 my-alpine
```

同时，该步骤也是**在线校验 `index.json` 是否生效**的手段：若返回失败，说明服务器 `index.json` 仍缺失/格式错误，Agent 后续运行时构建镜像会自动回退到上游 `images.linuxcontainers.org`，不影响既有已导入的本地 ready 镜像。

> Agent 运行时（`ensureReadyImage`）在 `incus_image_mirror` 非空时，会优先以 simplestreams 协议从该服务器拉取 alpine 基础镜像，失败自动回退上游；无需本机预先 `incus remote add`。

### 2. 定制 alpine 基础镜像

对于需要预装特定软件/内核参数的场景，可使用自己的 alpine-base 镜像（本地 tar.gz 或已 `incus image import` 的别名）：

```bash
# 安装时指定本地文件（导入为 incus 别名 podcctv/alpine-base，用于所有 alpine 容器）
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --alpine-base ./alpine-base.tar.gz

# 或直接传已存在的 incus 镜像别名
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --alpine-base my-alpine
```

> 定制 alpine-base 由你自行构建/托管（例如基于 https://github.com/podcctv/alpine-base ），与上面的 simplestreams 镜像服务器是两套独立资源。

### 3. SSH 登录欢迎页 / 横幅

预设：`none` / `default` / `minimal` / `project`（面向客户的可替换模板）。`custom` 配合 `--banner-text` 可完全自定义。

```bash
# 使用内置 project 模板（含控制面板/文档/技术支持占位信息，部署方可自行改写）
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --banner-preset project

# 完全自定义横幅文本
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --banner-preset custom --banner-text "$(cat /root/my-banner.txt)"
```

横幅会写入容器内的 `/etc/motd`（登录后）、`/etc/issue.net`（登录前）并通过 `sshd_config.d/99-runman.conf` 启用 `Banner`。

### 4. IPv6 精细化分配

默认每个容器分配 1 个 IPv6；通过 `--ipv6-alloc N` 可分配多个（例如出口需要多个独立 IPv6 的爬虫/代理场景）。

```bash
# 每个容器分配 10 个公网 IPv6 地址
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --ipv6-alloc 10
```

> 多地址以静态方式写入容器网络配置，并由宿主机 NDP 应答器逐一应答，保证每个地址均可达。

### 5. 纯 IPv6 容器

开启后，新建容器**不分配 IPv4、仅分配 IPv6**（nic 设 `ipv4.address=none`，cloud-init 仅写 `inet6`，DNS 改用 IPv6 解析器）。

```bash
# 要求 IPv6 模式为 subnet 或 snat（none 模式下会直接报错退出）
IPV6_MODE=subnet IPV6_ADDR=2001:db8::1 IPV6_SUBNET=2001:db8::/64 \
  bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh) \
  --ipv6-only
```

> 纯 IPv6 容器仅经 IPv6 可达，IPv4 NAT 端口转发不适用；请确认上游已正确路由该 IPv6 段到宿主机。

### 6. 强制 SSH 密码登录

无论使用上游镜像还是定制 alpine-base，Agent 在创建每个容器时都会注入 `99-runman.conf`（`PermitRootLogin yes` + `PasswordAuthentication yes`）并 `sed` 兜底主配置，确保容器内 root 密码登录始终可用。

---

## IPv6 配置备份与回滚

涉及网卡 / sysctl / incusbr0 的改动风险较高，本 Fork 提供**完备的备份—恢复—卸载恢复**机制：

- **变更前自动备份**：安装脚本在修改任何 IPv6/网卡配置**之前**，自动快照 `/etc/sysctl.d/99-narwhalcloud.conf`、`/etc/network/interfaces`、`incus network show incusbr0` 等到 `/var/lib/narwhal-agent/backups/ipv6-<时间戳>/`，并记录受管文件安装前是否已存在（`HAD_*` 标记）。
- **一键回滚**：

```bash
# 仅做 IPv6 配置备份（不改其它）
bash install.sh --backup-ipv6

# 回滚到最近一次备份
bash install.sh --rollback-ipv6

# 回滚到指定备份
/opt/narwhal-agent/ipv6-rollback.sh /var/lib/narwhal-agent/backups/ipv6-20260826-120000
```

> 卸载时会**首先**基于安装前备份恢复 IPv6/网卡配置，再清理其它文件，最大限度避免遗留错误路由影响业务。

---

## 一键卸载

```bash
# 撤销本安装器引入的全部变更（含 IPv6/网卡恢复），不影响既有业务
bash install.sh --uninstall
```

卸载流程严格有序：

1. **先恢复 IPv6 / 网卡配置**（基于安装前完整备份，最关键）；
2. `systemctl stop/disable` **仅本 Agent 服务**，不动其它业务服务；
3. 删除安装时新增的受管文件（`runman-incus.conf`、`ipv6-rollback.sh`）；
4. 清理 Agent 程序/配置目录，**保留 IPv6 备份目录**；
5. 日志明确提示：incus 容器/镜像及其它服务已保留。

> 如需同时清理 incus 制品，按日志提示显式执行：`incus network delete incusbr0`、`incus image delete <别名>`。

---

## 安装内容

| 组件          | 路径                                       |
|-------------|------------------------------------------|
| Agent 二进制   | `/opt/narwhal-agent/narwhal-agent`       |
| 配置文件        | `/opt/narwhal-agent/config.json`         |
| Agent 数据库   | `/opt/narwhal-agent/agent.db`            |
| 数据目录        | `/var/lib/narwhal-agent`                 |
| IPv6 备份目录   | `/var/lib/narwhal-agent/backups`         |
| IPv6 回滚辅助脚本 | `/opt/narwhal-agent/ipv6-rollback.sh`    |
| Podman 数据盘  | `/xfs_disk.img` → 挂载至 `/data`            |
| Systemd 服务  | `narwhal-agent.service`                  |
| rfw 防火墙（可选） | `/opt/narwhal-agent/rfw` + `rfw.service` |

**Web 管理面板**：`http://<服务器IP>:8792`

---

## 第三步 — 绑定 Token

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

---

## 更新 Agent

在已安装的服务器上重新执行同一命令，脚本会自动检测到已有安装并执行就地更新（Agent + netavark + rfw 同步更新，并补齐新增配置字段）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/podcctv/runman-agent/main/install.sh)
```

> 为避免覆盖你已配置的私有镜像源等参数，更新会**保留现有 `config.json`** 并仅补齐缺省字段（如 `incus_ipv6_only`）。

---

## 服务管理

```bash
# 查看 Agent 状态
systemctl status narwhal-agent

# 实时查看日志
journalctl -u narwhal-agent -f

# 重启 Agent
systemctl restart narwhal-agent

# 查看 rfw 防火墙状态
systemctl status rfw

# 重置面板密码（需重启生效）
/opt/narwhal-agent/narwhal-agent -reset-password 新密码
systemctl restart narwhal-agent
```

---

## 关键配置字段

`/opt/narwhal-agent/config.json`：

| 字段                 | 说明                             |
|--------------------|--------------------------------|
| `token`            | 母鸡 Token（安装完成后填入）              |
| `web`              | 面板监听地址（默认 `:8792`）             |
| `virt_type`        | `podman` / `cloudhv` / `incus` |
| `monitor_nic`      | 用于流量统计的网卡名（留空自动检测）             |
| `ipv6_mode`        | `none` / `snat` / `subnet`     |
| `max_port_forward` | 每个容器的最大端口转发规则数（默认 `20`）        |
| `incus_image_mirror` | 私有/本地镜像基址（覆盖 GitHub Releases），如 `https://alpine-incus-base.428048.xyz` |
| `incus_alpine_base` | 定制 alpine 基础镜像：本地 tar.gz 路径或已存在的 incus 别名 |
| `incus_ipv6_alloc` | 每个容器分配的 IPv6 数量（默认 `1`）          |
| `incus_ipv6_only`  | `true` 时新建容器为纯 IPv6（不分配 IPv4）      |
| `incus_banner_preset` | 欢迎页预设：`none`/`default`/`minimal`/`project`/`custom` |
| `incus_banner_text` | `preset=custom` 时的完整横幅文本           |
| `ipv6_backup_dir`  | IPv6 配置备份目录（默认 `/var/lib/narwhal-agent/backups`） |
| `ipv6_wg_subnet`   | 供 WireGuard 隧道分配的 IPv6 池（CIDR）       |

---

## 常见问题排查

**Agent 启动失败**
```bash
journalctl -u narwhal-agent --no-pager -n 50
```

**rfw 启动失败**
部分云厂商网卡不支持 eBPF。可以尝试在 rfw 服务启动参数中添加 `--xdp_mode skb`。

**Podman 数据盘未挂载**
```bash
mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
systemctl restart narwhal-agent
```

**KVM 不可用（cloud-hypervisor 模式）**
在宿主机管理界面开启嵌套虚拟化，或改用 Podman 模式。

**软件包安装失败**
脚本会自动重试 3 次并清理 dpkg 锁。若仍失败，手动执行 `apt-get update` 后再试。

**镜像拉取缓慢或失败（Incus 模式）**
- 改用私有镜像服务器：`--image-mirror https://alpine-incus-base.428048.xyz`
- 或完全离线：把镜像文件放入本地目录后用 `--local-image-dir <dir>` 安装
- 确认宿主机能访问 `images.linuxcontainers.org`（使用默认源时）

**卸载后 IPv6/网卡异常**
卸载会自动恢复安装前的备份；若仍有残留，可手动 `--rollback-ipv6` 或检查 `/var/lib/narwhal-agent/backups/` 下的快照。

**纯 IPv6 容器无法联网**
确认 `ipv6_mode` 为 `subnet` 或 `snat`（非 `none`），且上游已将该 IPv6 段路由到宿主机；纯 IPv6 容器无 IPv4，不能依赖 IPv4 NAT 端口转发。

---

> 本文档对应 `podcctv/runman-agent` 增强分支。更多实现细节与代码评审见 [`INCU_S_OPTIMIZATION_REPORT.md`](INCU_S_OPTIMIZATION_REPORT.md)。
