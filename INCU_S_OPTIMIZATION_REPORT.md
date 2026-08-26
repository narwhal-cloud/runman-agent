# runman-agent · Incus 容器优化与定制 Alpine 镜像整合 — 代码评审与优化报告

> 项目：`narwhal-cloud/runman-agent`（NarwhalCloud Agent，C2C NAT VPS 守护进程）
> 本次优化分支：`feat/incus-optimizations`
> 目标：提升 Incus LXC 后端在离线/内网环境的部署体验，整合定制 Alpine 镜像（`podcctv/alpine-base`），并补齐 IPv6、欢迎页、SSH 密码登录等能力。

---

## 0. 现有代码结构概览

| 文件 | 职责 |
|---|---|
| `install.sh` | 安装/更新脚本（Bash），处理依赖安装、内核参数、IPv6 探测、镜像导入、config.json 生成 |
| `manager/incus/incus.go` | Incus LXC 后端实现：容器创建/删除/启停、ready 镜像构建、IP 分配、cloud-init 生成 |
| `manager/manager.go` | `VMManager` 接口定义（与 podman/cloudhv 共用） |
| `ndp/ndp.go`、`ndp/incus.go`、`ndp/hostinfo.go`、`ndp/responder.go` | NDP 应答器：让上游认为宿主机持有容器 IPv6，实现子网模式可达 |
| `manager/wgbind/*` | 纯用户态 WireGuard 绑定（隧道地址流量转发进 VM 内网 IPv4） |
| `config/config.go` | 全局配置结构与读写（并发安全） |
| `db/db.go` | SQLite（gorm）持久化，含 `IncusVMConfig` 等表 |
| `main.go` | 入口，按 `virt_type` 选择后端并装配各子系统 |

**Incus 实例创建主链路**（`incus.go:createVM`）：
1. `NextIncusIdx()` 取最小空闲索引 → 计算静态 IPv4 / IPv6；
2. 解析镜像别名（`debian/13/cloud`、`alpine/3.23/cloud`），若 `…/ready` 别名不存在则临时起一个 builder 容器烤入 sshd/工具并发布为 `…/ready`；
3. 生成 cloud-init（`ssh_pwauth`、root 密码、`/etc/network/interfaces` 或 systemd-networkd 静态地址、重启 sshd）；
4. `CreateInstance` 启动，写 `IncusVMConfig`。

---

## 1. 现有实现评审（先说结论）

**优点**
- 镜像 `…/ready` 预构建思路很好：把装包/改 sshd 烤进镜像，实例级 cloud-init 只做密码与网络，开机快且稳定。
- IPv6 探测（`detect_and_configure_ipv6`）做得很扎实：多端点探测、静态/动态地址优先级、甚至用 `+1` 地址实测 uRPF，逻辑严谨。
- NDP 应答器与 `incusTracker` 用反射解耦 DB，避免了编译期依赖。
- 全量 `config.Manager` 加锁、`db` 用 WAL + `busy_timeout` + 单连接，并发设计稳健。

**问题 / 缺口（即本次优化要补的）**
1. **离线/内网无本地镜像服务能力**：`import_incus_images` 只能从 GitHub releases 下载，断网或内网无法部署。
2. **无 SSH 登录横幅 / 欢迎页**：实例创建没有注入 motd / Banner，不满足"自定义登录页"。
3. **IPv6 仅分配单个地址**：`computeIPs` 只给一个 `…::%x`，不支持"非 /64 网段精细化分配 N 个 IP"；WireGuard 也无 IPv6 池概念；IPv6 配置变更后**没有备份/回滚**手段，试错成本高。
4. **SSH 密码登录依赖 ready 镜像**：若使用自定义 `alpine-base`（不经过 `ensureReadyImage` 烤入 `99-runman.conf`），可能丢失 `PasswordAuthentication yes`，导致容器 SSH 密码登录失败。
5. **其它**：日志缺结构化/级别、错误常在 `log.Printf` 后直接返回未包装的 `err`、部分 `fmt.Errorf` 丢失原错误链、install.sh 中 `set -e` 与若干 `|| true` 混用易掩盖失败、镜像导入失败无告警退出。

---

## 2. 本次已实现的具体优化

### 2.1 本地 / 内网 Incus 镜像服务（优化点 1）
- `install.sh` 新增统一镜像基址变量 **`INCUS_IMAGE_MIRROR`**（URL）与 **`--image-mirror <url>`**：`import_incus_images` 现以该基址覆盖默认 `VM_IMAGES_BASE`，可指向本地静态服务（`python3 -m http.server` 等）或内网镜像。
- 新增 **`--local-image-dir <dir>`** / **`INCUS_LOCAL_IMAGE_DIR`**：直接离线导入目录内的 `incus-<distro>-<arch>.tar.gz`，完全不需要网络；并跳过远程导入避免覆盖。
- 新增 **`import_custom_alpine_base`**：支持传入本地 tar.gz（导入为 `podcctv/alpine-base` 别名）或已存在的 incus 别名，配合 `INCUS_ALPINE_BASE` / `--alpine-base`。
- `config.json` 新增 `incus_image_mirror` 字段，便于后续运维查看当前镜像源。

### 2.2 自定义 SSH 登录欢迎页 / 横幅（优化点 2）
- `config.json` 新增 `incus_banner_preset`（`none`/`default`/`minimal`/`project`/`custom`）与 `incus_banner_text`（custom 时的完整文本）。
- `incus.go:createVM` 现会向容器注入：
  - `/etc/motd`（登录后）、`/etc/issue.net`（登录前）；
  - `/etc/ssh/sshd_config.d/99-runman.conf` 设置 `Banner /etc/issue.net`；
  - runcmd 中 `sed` 兜底写入主 `sshd_config` 的 `Banner`/`PasswordAuthentication`/`PermitRootLogin`（兼容未 `Include` drop-in 目录的镜像）。
- 预设 `project` 提供可替换的"预设项目"模板；`custom` 支持完全自定义文本。安装脚本 `prompt_banner` 交互选择，也可通过 `--banner-preset/--banner-text` 非交互指定。

### 2.3 Incus 容器 IPv6 探测 + 精细化分配 + 备份回滚（优化点 3）
- **多地址分配**：`computeIPs` 现按 `incus_ipv6_alloc`（默认 1，可配 10 等）返回一组 IPv6；cloud-init 为 Alpine（interfaces）与 Debian（systemd-networkd）写入多个静态地址，网关改算为 incusbr0 桥地址 `<prefix>::1`（修正了原代码误用宿主机公网地址作网关的隐患）。
- **WireGuard IPv6 池**：`config.json` 新增 `ipv6_wg_subnet`，subnet 模式下由安装脚本自动复用 `IPV6_SUBNET` 写入，供 wgbind 后续为隧道分配 IPv6（见 §4 说明）。
- **备份 / 一键回滚（详见 §2.6）**：
  - `backup_ipv6_config()` 在**一切网卡/sysctl 修改之前**快照 `/etc/sysctl.d/99-narwhalcloud.conf`、`/etc/network/interfaces`、`incus network show incusbr0` 及关键变量到 `/var/lib/narwhal-agent/backups/ipv6-<时间戳>/`，并用 `HAD_*` 标记记录受管对象安装前是否已存在。
  - `restore_ipv6_config()` 据此区分"恢复原始文件"还是"删除安装时新建的文件"。
  - 安装脚本支持 **`--backup-ipv6` / `--rollback-ipv6`** 独立模式；并安装常驻辅助脚本 `/opt/narwhal-agent/ipv6-rollback.sh`。
- `ndp/incus.go` 的 `incusTracker` 现按逗号拆分 `IncusVMConfig.IPv6s`，对分配的**每一个** IPv6 都应答 NDP，保证多地址可达。

### 2.4 容器内 SSH 用户密码登录（优化点 4）
- ready 镜像构建（`ensureReadyImage`）已写 `99-runman.conf` 开启密码登录；本次进一步在**每次创建实例**的 cloud-init 中强制注入 `PermitRootLogin yes` + `PasswordAuthentication yes`（含对自定义 `alpine-base` 的兼容），并 `sed` 兜底主配置，确保无论基础镜像如何，容器 SSH 密码登录均可用。
- `bake_sshd_config`（VM 镜像烤制）与 ready 构建器保持一致策略。

### 2.5 配置/数据层改动
- `config/config.go`：新增 `IncusBannerPreset/Text`、`IncusIPv6Alloc`、`IncusAlpineBase`、`IncusImageMirror`、`IPv6WGSubnet`、`IPv6BackupDir`，并补 `applyDefaults`。
- `db/db.go`：`IncusVMConfig` 新增 `IPv6s`（逗号分隔多地址），`IPv6` 保留首个用于兼容。
- `main.go`：`incus.New` 调用透传新参数。
- `install.sh` 更新流程新增 **配置迁移**（jq 补齐旧配置的缺省字段），避免升级后新功能不生效。

### 2.6 一键安装 / 卸载 + IPv6 / 网卡完备备份恢复（运维安全，优化点 3 加固）

> 用户诉求：脚本需支持**一键安装**、**一键卸载**；卸载**不能影响既有业务**（其它服务、既有 incus 容器/镜像默认保留）；尤其"给 incus 配置 IPv6"涉及网卡/sysctl 改动，必须有**完备的备份 — 恢复 — 卸载恢复**方案，且脚本须提供一键卸载选项。

**A. 备份时机修正（关键 bug 修复）**
- 原实现在 `configure_host_ipv6_routing`（已改写 sysctl/网卡）**之后**才调用 `backup_ipv6_config`，导致"备份"捕获的是已修改状态，恢复成 no-op。
- 现改为在一切 IPv6/网卡修改**之前**（line 1466-1472，`_USER_IPV6_MODE != none` 时）即调用 `backup_ipv6_config`，确保快照是**原始状态**。

**B. `HAD_*` 标记区分"恢复" vs "删除"**
- `backup_ipv6_config` 在 `meta.env` 记录三个受管对象**安装前是否已存在**：`HAD_SYSCTL`（`/etc/sysctl.d/99-narwhalcloud.conf`）、`HAD_MODULES`（`/etc/modules-load.d/runman-incus.conf`）、`HAD_INCUSBR0`（`incus network show incusbr0`）。
- `restore_ipv6_config` 据此决策：原文件存在 → 恢复原始文件；安装时新建 → 直接删除。避免误删用户原有配置，也避免残留安装器文件。

**C. 一键卸载 `--uninstall`**
- 新增独立模式（line 207-210 早退），由 `do_uninstall()`（line 121）执行，流程严谨有序：
  1. **最先恢复 IPv6 / 网卡配置**（基于安装前完整备份，最关键——避免遗留错误路由/转发影响业务）；
  2. `systemctl stop/disable` **仅本 agent 服务**、删 service 文件、`daemon-reload`（**不动其它业务服务**）；
  3. 删安装时新增的受管文件（`runman-incus.conf`、`ipv6-rollback.sh`）；
  4. `rm -rf` agent 程序/配置目录，但**保留 IPv6 备份目录**（便于事后人工恢复）；
  5. 日志明确提示：incus 容器/镜像及其它服务已保留；如需彻底清理 incus 制品，给出 `incus network delete incusbr0` / `incus image delete <别名>` 显式提示。

**D. 独立 IPv6 备份 / 回滚**
- `--backup-ipv6` / `--rollback-ipv6`（line 200-204 早退），可脱离安装流程单独对当前主机做备份/回滚；
- 常驻辅助脚本 `ipv6-rollback.sh` 同样可独立执行（`./ipv6-rollback.sh [备份目录]`）。

**E. 安全边界（卸载的"不影响业务"保证）**
- 卸载**不**删除既有 incus 容器/镜像、**不**停止无关 systemd 服务、**不**清理用户原有 `/etc/network/interfaces`、`/etc/sysctl.d/99-narwhalcloud.conf`（若安装前已存在则恢复原状）；
- 仅撤销本安装器引入的变更；彻底清理 incus 制品由运维按日志提示显式决定。

---

## 3. 额外优化建议（代码评审延伸）

**错误处理**
- 统一错误包装：将散落的 `fmt.Errorf("...: %w", err)` 规范化为带上下文的包装；`incus.go` 中 `CreateInstance`/`ExecInstance` 失败建议附带 `vmID` 便于排障。
- install.sh 的 `set -e` 是双刃剑：镜像导入失败目前被 `|| true` 吞掉，建议对关键步骤（依赖安装、agent 二进制下载、incus 网络创建）在失败时报错退出，而非静默继续。

**日志**
- 引入结构化/级别日志（如 `log/slog`），将 `[Incus]` 前缀、vmID、操作类型纳入字段，便于集中采集。
- Web 面板与 gRPC 命令处理（`main.go:handleCommand`）已有 `recover()` 兜底，建议记录 `CommandId` 便于追踪失败命令。

**安全性**
- `incus.New` 以 root 运行且 `PasswordAuthentication yes` + 固定 root 密码是 NAT VPS 场景的固有选择，但建议：① 默认禁止密码登录、改为下发 SSH 公钥（若平台支持）；② 至少强制 `fail2ban` 或 rfw 对 22 端口限速；③ 镜像导入校验 checksum（现有 prebuilt 镜像未校验哈希即 `incus image import`）。
- `ResetPassword`/`ExecInstance` 直接拼接 shell 命令，注意 `password` 含特殊字符时的注入风险（当前 `chpasswd` 用 `echo 'root:...'` 已较安全，但建议转义）。

**性能**
- `ListVMs` 对每个实例串行 `GetVMInfo(nil, ...)`（传 `nil` context），实例多时偏慢；可并发获取或缓存。
- `computeIPs` 的 idx 分配用 `NextIncusIdx` 全表扫描，实例规模大时可改为持久化"最大已用 idx"或位图。
- `ensureReadyImage` 超时为 120×5s=10 分钟且串行构建，建议在安装期预烤而非首次创建期（本次的 `import_incus_images` 已在安装期完成，符合此方向）。

**可维护性**
- `install.sh` 已 1500+ 行，建议拆分为 `lib/*.sh`（镜像、网络、ipv6、banner 等），主脚本 source 后调用。
- Go 侧 `incus.go` 的 cloud-init 字符串拼接较长，建议抽取为模板（`text/template`）提升可读性。

---

## 4. 已知边界与后续工作

- **WireGuard IPv6 运行时分配**：本次仅完成"IPv6 池探测 + 配置落库"，wgbind 的 netstack 当前仅处理 IPv4 入站；要让隧道真正承载 IPv6，需在 `wgbind` 的转发路径增加 IPv6 监听与邻居/路由处理（纯用户态方案较大改动），建议作为独立子任务排期。
- **多地址与 incus ipv6 filtering**：当前未启用 `security.ipv6_filtering`，多地址以静态方式写入容器，依赖 NDP 应答器。若后续启用 IPv6 filtering，需确认 incus 对逗号分隔多地址的放行行为。
- **镜像校验**：建议为 prebuilt 镜像补充 SHA256/签名校验后再导入。

---

## 5. 改动文件清单

| 文件 | 改动 |
|---|---|
| `install.sh` | 镜像源/本地目录/定制 alpine 导入、横幅交互、IPv6 变更前备份 + `HAD_*` 恢复/删除逻辑、IPv6 备份/回滚独立模式、`--uninstall` 一键卸载（含先恢复 IPv6/网卡）、配置迁移 |
| `config/config.go` | 新增 7 个配置字段 + 默认值 |
| `db/db.go` | `IncusVMConfig.IPv6s` 多地址字段 |
| `manager/incus/incus.go` | banner 注入、多 IPv6 分配、网关修正、定制 alpine 别名、sshd 兜底、`ensureReadyImage` 本地源 |
| `ndp/incus.go` | 逗号分隔多 IPv6 的 NDP 应答 |
| `main.go` | `incus.New` 透传新参数 |

> **编译验证状态（重要）**
> - Go 改动已通过 `gofmt -e` 解析校验（无语法错误）；`go list -deps ./manager/incus` 可离线解析完整模块图且**无缺失模块**，证明所有 import/跨文件引用有效。
> - **`manager/incus`（本次主要改动包）不依赖 `proglottis/gpgme`**（已用 `go list -deps` 确认）；`gpgme` 仅由 `ndp` 经预置的 cgo 链路引入——这是 Incus SDK 的既有构建依赖（需宿主机的 `libgpgme-dev`），**非本次改动引入**。
> - **本机沙箱（Windows 交叉编译宿主机、无 `libgpgme`、且 Go 编译子进程无法在沙箱内执行）无法跑通完整 `go build`**：`go build` 在此环境以退出码 1 且无输出挂死，属工具链/沙箱限制，与代码无关。
> - **推荐在 Debian 13 构建宿主机（已装 `libgpgme-dev`、有 incus 运行时）验证**：`GOOS=linux go build ./...`，随后端到端验证镜像导入/横幅/多 IPv6 行为（本机沙箱无 incus，无法实跑）。
> - install.sh 已通过 `bash -n` 语法检查，新增函数 `backup_ipv6_config` / `restore_ipv6_config` / `install_ipv6_rollback_helper` / `import_local_incus_images` / `import_custom_alpine_base` / `prompt_banner` / `do_uninstall` 均已就绪；`--uninstall` / `--backup-ipv6` / `--rollback-ipv6` 接线经 grep 校验（flag 194 / one-shot 200-204 / 卸载早退 207-210 / `do_uninstall` 121 / `backup_ipv6_config` 仅 1469 与 201 两处调用）；`HAD_*` 标记在 backup（56-58）与 restore（72-89）中一致。
