#!/bin/bash
set -e
export DEBIAN_FRONTEND=noninteractive

# ────────────────────────────────────────────────────────────────────────────────
# NarwhalCloud Agent install / update script
# ────────────────────────────────────────────────────────────────────────────────

AGENT_BIN_DIR="/opt/narwhal-agent"
AGENT_BINARY="$AGENT_BIN_DIR/narwhal-agent"
AGENT_SERVICE="narwhal-agent"
AGENT_DATA_DIR="/var/lib/narwhal-agent"
AGENT_CONFIG_DIR="/opt/narwhal-agent"
AGENT_CONFIG_FILE="$AGENT_CONFIG_DIR/config.json"
AGENT_DB="$AGENT_CONFIG_DIR/agent.db"
AGENT_WEB_PORT="8792"
RFW_BIN_DIR="$AGENT_BIN_DIR"   # rfw 与 agent 放在同一目录，匹配面板 rfwBinaryPath()
RFW_API_ADDR="127.0.0.1:7734"  # rfw 仅监听本地，由 agent 面板反代
PODMAN_NETWORK="narwhal-net"

DOWNLOAD_BASE="https://github.com/narwhal-cloud/runman-agent/releases/latest/download"
CLOUD_HYPERVISOR_BASE="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest/download"
# 预构建系统镜像（cloudhv/incus），由 narwhal-cloud/images 仓库 CI 每月构建
VM_IMAGES_BASE="https://github.com/narwhal-cloud/images/releases/download/vm-latest"
NETAVARK_BASE="https://github.com/narwhal-cloud/netavark/releases/latest/download"
# rfw v2 随 agent 同仓库发布，资产名为 rfw-amd64 / rfw-arm64
RFW_BASE="$DOWNLOAD_BASE"

# t EN ZH — returns the string for current language
t() { [ "$LANG_CODE" = "zh" ] && echo "$2" || echo "$1"; }

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"; }
die() { log "$1" >&2; exit 1; }

# ── IPv6 配置备份 / 一键回滚 ───────────────────────────────────────────────────
# 备份对象：内核参数(sysctl)、/etc/network/interfaces、incusbr0 网络配置、关键变量。
# 回滚时按备份快照逐一恢复，便于本机/容器 IPv6 分配策略试错后快速还原。

backup_ipv6_config() {
    local ts; ts=$(date +%Y%m%d-%H%M%S)
    local dir="$INCUS_IPV6_BACKUP_DIR/ipv6-$ts"
    mkdir -p "$dir"
    # 仅在“变更前”调用：复制当前（原始）状态，并记录受管文件安装前是否已存在，
    # 供卸载/回滚时区分“恢复原始文件”还是“删除安装时新建的文件”。
    [ -f /etc/sysctl.d/99-narwhalcloud.conf ] && cp /etc/sysctl.d/99-narwhalcloud.conf "$dir/sysctl.conf"
    [ -f /etc/network/interfaces ] && cp /etc/network/interfaces "$dir/interfaces"
    command -v incus >/dev/null 2>&1 && incus network show incusbr0 >"$dir/incusbr0.network" 2>/dev/null
    [ -f /etc/modules-load.d/runman-incus.conf ] && cp /etc/modules-load.d/runman-incus.conf "$dir/runman-incus.conf"
    {
        echo "IPV6_MODE=${IPV6_MODE:-}"
        echo "IPV6_SUBNET=${IPV6_SUBNET:-}"
        echo "IPV6_ADDR=${IPV6_ADDR:-}"
        echo "IPV6_IFACE=${IPV6_IFACE:-}"
        echo "INCUS_WG_IPV6_SUBNET=${INCUS_WG_IPV6_SUBNET:-}"
        # 关键：记录受管文件/网络在安装前是否已存在（1=已存在需恢复，0=安装时新建需删除）
        echo "HAD_SYSCTL=$( [ -f /etc/sysctl.d/99-narwhalcloud.conf ] && echo 1 || echo 0 )"
        echo "HAD_MODULES=$( [ -f /etc/modules-load.d/runman-incus.conf ] && echo 1 || echo 0 )"
        echo "HAD_INCUSBR0=$( command -v incus >/dev/null 2>&1 && incus network show incusbr0 >/dev/null 2>&1 && echo 1 || echo 0 )"
    } > "$dir/meta.env"
    echo "$dir" > "$INCUS_IPV6_BACKUP_DIR/latest"
    echo "$dir"
}

restore_ipv6_config() {
    local target="${1:-$(cat "$INCUS_IPV6_BACKUP_DIR/latest" 2>/dev/null)}"
    [ -n "$target" ] && [ -d "$target" ] || { log "$(t "No IPv6 backup found" "未找到 IPv6 备份")"; return 1; }
    # 载入备份元数据（含 HAD_* 标记）
    [ -f "$target/meta.env" ] && . "$target/meta.env"
    # sysctl：有原文件则恢复；无原文件（安装时新建）则直接删除
    if [ -f "$target/sysctl.conf" ]; then
        cp "$target/sysctl.conf" /etc/sysctl.d/99-narwhalcloud.conf && sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1
    elif [ "${HAD_SYSCTL:-0}" = "0" ] && [ -f /etc/sysctl.d/99-narwhalcloud.conf ]; then
        rm -f /etc/sysctl.d/99-narwhalcloud.conf && sysctl --system >/dev/null 2>&1
        log "$(t "Removed install-created sysctl file" "已删除安装时新建的 sysctl 配置文件")"
    fi
    # interfaces：仅当备份存在时恢复（避免误删用户原有配置）
    [ -f "$target/interfaces" ] && cp "$target/interfaces" /etc/network/interfaces
    # incusbr0 网络：原存在则恢复，否则（安装时新建）删除
    if command -v incus >/dev/null 2>&1; then
        if [ "${HAD_INCUSBR0:-1}" = "1" ] && [ -f "$target/incusbr0.network" ]; then
            incus network edit incusbr0 <"$target/incusbr0.network" 2>/dev/null \
                && log "$(t "Restored incusbr0 network" "已恢复 incusbr0 网络配置")"
        elif [ "${HAD_INCUSBR0:-1}" = "0" ]; then
            incus network delete incusbr0 2>/dev/null \
                && log "$(t "Removed incusbr0 (created by installer)" "已删除安装时创建的 incusbr0")"
        fi
    fi
    # modules-load（安装时新建则删除）
    if [ "${HAD_MODULES:-0}" = "0" ] && [ -f /etc/modules-load.d/runman-incus.conf ]; then
        rm -f /etc/modules-load.d/runman-incus.conf
        log "$(t "Removed runman-incus modules config" "已删除 runman-incus 内核模块配置")"
    fi
    log "$(t "IPv6 config restored from $target" "已从 $target 回滚 IPv6 配置")"
}

# 生成独立的 IPv6 回滚脚本，供运维手动执行（无需重跑安装脚本）
install_ipv6_rollback_helper() {
    mkdir -p "$(dirname "$AGENT_BINARY")"
    cat > "$AGENT_BIN_DIR/ipv6-rollback.sh" <<'EOF'
#!/bin/bash
# NarwhalCloud Agent — IPv6 配置一键回滚
# 用法：
#   ./ipv6-rollback.sh              # 回滚到最近一次备份
#   ./ipv6-rollback.sh <备份目录>   # 回滚到指定备份（/var/lib/narwhal-agent/backups/ipv6-xxx）
set -e
BACKUP_DIR="${INCUS_IPV6_BACKUP_DIR:-/var/lib/narwhal-agent/backups}"
TARGET="${1:-$(cat "$BACKUP_DIR/latest" 2>/dev/null)}"
[ -n "$TARGET" ] && [ -d "$TARGET" ] || { echo "未找到 IPv6 备份"; exit 1; }
[ -f "$TARGET/sysctl.conf" ] && { cp "$TARGET/sysctl.conf" /etc/sysctl.d/99-narwhalcloud.conf && sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1; }
[ -f "$TARGET/interfaces" ] && cp "$TARGET/interfaces" /etc/network/interfaces
[ -f "$TARGET/incusbr0.network" ] && command -v incus >/dev/null 2>&1 && incus network edit incusbr0 <"$TARGET/incusbr0.network" 2>/dev/null
echo "已回滚 IPv6 配置: $TARGET"
EOF
    chmod +x "$AGENT_BIN_DIR/ipv6-rollback.sh"
    log "$(t "IPv6 rollback helper installed: $AGENT_BIN_DIR/ipv6-rollback.sh" "IPv6 回滚辅助脚本已安装: $AGENT_BIN_DIR/ipv6-rollback.sh")"
}

# ── 一键卸载 ────────────────────────────────────────────────────────────────
# 仅撤销本安装器引入的变更，不影响既有业务（其它服务、incus 容器/镜像默认保留）。
# 最关键的是先恢复 IPv6 / 网卡配置（基于安装前的完整备份），再停止服务、清理文件。
do_uninstall() {
    [ "$(id -u)" = "0" ] || die "$(t "Uninstall requires root." "卸载需要 root 权限。")"
    log "$(t "=== NarwhalCloud Agent uninstall ===" "=== NarwhalCloud Agent 卸载 ===")"

    # 0. 最先恢复 IPv6 / 网卡配置（最关键：避免遗留错误路由/转发影响业务）
    if [ -d "$INCUS_IPV6_BACKUP_DIR" ]; then
        restore_ipv6_config || log "$(t "IPv6 restore skipped (no backup)." "IPv6 恢复跳过（无备份）。")"
    fi

    # 1. 停止并禁用服务（仅本 agent，不动其它业务服务）
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "$AGENT_SERVICE" 2>/dev/null || true
        systemctl disable "$AGENT_SERVICE" 2>/dev/null || true
    fi
    rm -f "/etc/systemd/system/${AGENT_SERVICE}.service"
    systemctl daemon-reload 2>/dev/null || true

    # 2. 删除安装时新增/修改的受管文件（restore 已按备份删除新建项，这里兜底）
    rm -f /etc/modules-load.d/runman-incus.conf
    rm -f "$AGENT_BIN_DIR/ipv6-rollback.sh"

    # 3. 删除 agent 程序/配置目录，但保留 IPv6 备份（便于事后人工恢复）
    rm -rf "$AGENT_BIN_DIR" "$AGENT_CONFIG_DIR"
    if [ -d "$AGENT_DATA_DIR" ]; then
        for f in "$AGENT_DATA_DIR"/*; do
            [ -e "$f" ] || continue
            [ "$f" = "$INCUS_IPV6_BACKUP_DIR" ] && continue
            rm -rf "$f"
        done
    fi

    log "$(t "Uninstall complete. Incus containers/images and other services are preserved." "卸载完成。已保留 incus 容器/镜像及其它业务服务。")"
    log "$(t "IPv6 backups kept at: $INCUS_IPV6_BACKUP_DIR (use ipv6-rollback.sh or --rollback-ipv6 to restore)" "IPv6 备份保留于: $INCUS_IPV6_BACKUP_DIR（可用 ipv6-rollback.sh 或 --rollback-ipv6 恢复）")"
    log "$(t "To also remove incus artifacts: incus network delete incusbr0 ; incus image delete <alias>" "如需同时清理 incus：incus network delete incusbr0；incus image delete <别名>")"
}

# ── Language selection ────────────────────────────────────────────────────────

# Support non-interactive mode via environment variable or command line argument
INSTALL_RFW_FORCE=0
# 1: 忽略版本戳，强制重新下载 cloudhv / incus 预构建镜像
FORCE_IMAGE_REFRESH="${FORCE_IMAGE_REFRESH:-0}"

# 本地 incus 镜像服务 / 定制能力相关环境变量（离线/内网部署用）
INCUS_IMAGE_MIRROR="${INCUS_IMAGE_MIRROR:-https://alpine-incus-base.428048.xyz}"  # 私有 simplestreams 镜像服务器（fork 默认）；留空则使用 GitHub releases
INCUS_LOCAL_IMAGE_DIR="${INCUS_LOCAL_IMAGE_DIR:-}" # 本地镜像目录（含 incus-<distro>-<arch>.tar.gz），直接离线导入
INCUS_ALPINE_BASE="${INCUS_ALPINE_BASE:-}"         # 定制 alpine 基础镜像：本地 tar.gz 路径或已存在的 incus 别名
INCUS_IPV6_ALLOC="${INCUS_IPV6_ALLOC:-1}"          # 每个容器分配的 IPv6 数量（非 /64 网段精细化分配）
INCUS_WG_IPV6_SUBNET="${INCUS_WG_IPV6_SUBNET:-}"   # 供 WireGuard 隧道分配的 IPv6 池（CIDR）
INCUS_BANNER_PRESET="${INCUS_BANNER_PRESET:-none}" # none / default / minimal / project / custom
INCUS_BANNER_TEXT="${INCUS_BANNER_TEXT:-}"         # preset=custom 时的完整横幅文本
INCUS_IPV6_BACKUP_DIR="${INCUS_IPV6_BACKUP_DIR:-/var/lib/narwhal-agent/backups}" # IPv6 配置备份目录
INCUS_IPV6_ONLY="${INCUS_IPV6_ONLY:-}" # 设为 1 时新建容器为纯 IPv6（不分配 IPv4），需 IPv6 模式为 subnet/snat

# 一键模式：--backup-ipv6 / --rollback-ipv6 直接对当前主机做 IPv6 配置备份/回滚后退出
IPV6_ONESHOT_MODE="${IPV6_ONESHOT_MODE:-}"
# 一键卸载模式：--uninstall 撤销本安装器引入的全部变更（含 IPv6 / 网卡恢复）后退出
UNINSTALL="${UNINSTALL:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        zh) LANG_CODE="zh"; shift ;;
        en) LANG_CODE="en"; shift ;;
        --install-rfw) INSTALL_RFW_FORCE=1; shift ;;
        --force-images) FORCE_IMAGE_REFRESH=1; shift ;;
        --image-mirror) INCUS_IMAGE_MIRROR="$2"; shift 2 ;;
        --local-image-dir) INCUS_LOCAL_IMAGE_DIR="$2"; shift 2 ;;
        --alpine-base) INCUS_ALPINE_BASE="$2"; shift 2 ;;
        --ipv6-alloc) INCUS_IPV6_ALLOC="$2"; shift 2 ;;
        --wg-ipv6-subnet) INCUS_WG_IPV6_SUBNET="$2"; shift 2 ;;
        --banner-preset) INCUS_BANNER_PRESET="$2"; shift 2 ;;
        --banner-text) INCUS_BANNER_TEXT="$2"; shift 2 ;;
        --ipv6-only) INCUS_IPV6_ONLY=1; shift ;;
        --backup-ipv6) IPV6_ONESHOT_MODE="backup"; shift ;;
        --rollback-ipv6) IPV6_ONESHOT_MODE="rollback"; shift ;;
        --uninstall) UNINSTALL=1; shift ;;
        *) shift ;;
    esac
done

# ── 一键 IPv6 备份 / 回滚（独立模式，处理完即退出）──
if [ "$IPV6_ONESHOT_MODE" = "backup" ]; then
    b=$(backup_ipv6_config); log "$(t "IPv6 config backed up to $b" "IPv6 配置已备份至 $b")"; exit 0
elif [ "$IPV6_ONESHOT_MODE" = "rollback" ]; then
    restore_ipv6_config; exit 0
fi

# ── 一键卸载（独立模式，处理完即退出）──
if [ "$UNINSTALL" = "1" ]; then
    do_uninstall
    exit 0
fi

if [ -z "$LANG_CODE" ]; then
    printf "Select language / 选择语言:\n  1) English (default)\n  2) 中文\n"
    read -t 5 -rp "> " _lang_choice || _lang_choice=1
    case "${_lang_choice}" in
        2) LANG_CODE="zh" ;;
        *) LANG_CODE="en" ;;
    esac
fi

log "$(t "Recommended OS: Debian 13 (Trixie) for best compatibility." "推荐操作系统：使用 Debian 13 (Trixie) 以获得最佳兼容性。")"

# ────────────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────────────

cleanup_locks() {
    killall apt apt-get dpkg 2>/dev/null || true
    rm -f /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
    dpkg --configure -a 2>/dev/null || true
}

install_packages() {
    local virt_type="${1:-podman}"
    local max=3 attempt=1
    local base_packages="curl wget python3 uuid-runtime systemd-zram-generator jq chrony iptables iproute2 fdisk bc dnsutils"
    local extra_packages

    if [ "$virt_type" = "cloudhv" ]; then
        extra_packages="qemu-utils cloud-image-utils zstd"
    elif [ "$virt_type" = "incus" ]; then
        extra_packages="incus uidmap acl bridge-utils"
    else
        extra_packages="podman lxcfs xfsprogs"
    fi

    while [ $attempt -le $max ]; do
        log "$(t "Installing packages (attempt $attempt)..." "安装软件包 (第 $attempt 次)...")"
        if apt-get update -qq && apt-get install -y $base_packages $extra_packages; then
            log "$(t "Packages installed." "软件包安装成功。")"
            return 0
        fi
        cleanup_locks
        [ $attempt -eq $max ] && { log "$(t "Package installation failed." "软件包安装失败。")"; exit 1; }
        attempt=$((attempt + 1))
        sleep 5
    done
}

start_service() {
    systemctl daemon-reload
    local svc=$1
    systemctl is-active --quiet "$svc" || systemctl start "$svc"
    systemctl is-enabled --quiet "$svc" || systemctl enable "$svc"
    log "$(t "Service $svc started." "服务 $svc 已启动。")"
}

check_podman_version() {
    if ! command -v podman &>/dev/null; then
        log "$(t "Podman not found." "未找到 Podman。")"
        exit 1
    fi
    local version_str
    version_str=$(podman -v | awk '{print $3}')
    log "$(t "Detected Podman version: $version_str" "检测到 Podman 版本: $version_str")"

    # Compare version $version_str with 5.4.2
    local IFS=.
    local cur=($version_str)
    local min=(5 4 2)
    for i in 0 1 2; do
        if [[ ${cur[i]:-0} -lt ${min[i]} ]]; then
            log "$(t "Error: Podman version must be >= 5.4.2. Current version is $version_str" "错误: Podman 版本必须 >= 5.4.2。当前版本为 $version_str")"
            log "$(t "It is highly recommended to use Debian 13 (Trixie) to get the latest Podman." "强烈建议使用 Debian 13 (Trixie) 以获取最新版本的 Podman。")"
            exit 1
        elif [[ ${cur[i]:-0} -gt ${min[i]} ]]; then
            return 0
        fi
    done
    return 0
}

download_with_retry() {
    local url=$1 dest=$2 max=3 attempt=1
    while [ $attempt -le $max ]; do
        log "$(t "Downloading $url (attempt $attempt)..." "下载 $url (第 $attempt 次)...")"
        if curl -fsSL -o "$dest" "$url"; then return 0; fi
        [ $attempt -eq $max ] && { log "$(t "Download failed." "下载失败。")"; return 1; }
        attempt=$((attempt + 1)); sleep 5
    done
}

detect_arch() {
    case $(uname -m) in
        x86_64)        echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) log "$(t "Unsupported architecture: $(uname -m)" "不支持的架构: $(uname -m)")"; exit 1 ;;
    esac
}

# ── IPv6 helpers ──────────────────────────────────────────────────────────────

ipv6_plus_one() {
    python3 -c "import ipaddress; print(str(ipaddress.IPv6Address('$1') + 1))" 2>/dev/null
}

# Returns "none" or "IFACE|ADDR|PREFIX"
detect_and_configure_ipv6() {
    log "$(t "Detecting public IPv6..." "开始检测公网 IPv6...")" >&2
    # 多厂商：ip.sb 在境内或某些时段不可达，依次尝试多个公共端点；
    # 全部失败但默认路由存在时，仍按"内部 IPv6 可用"继续，避免误判。
    local connected=0 endpoint
    for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com www.google.com; do
        if curl -6 -s --max-time 5 "$endpoint" >/dev/null 2>&1; then
            connected=1; break
        fi
    done
    if [ "$connected" = "0" ]; then
        if ip -6 route show default 2>/dev/null | grep -q .; then
            log "$(t "External IPv6 probe failed but default route exists, continuing." "公网 IPv6 探测失败但存在默认路由，继续配置。")" >&2
        else
            log "$(t "No public IPv6 detected." "未检测到公网 IPv6。")" >&2
            echo "none"; return 0
        fi
    else
        log "$(t "✓ Public IPv6 available." "✓ 公网 IPv6 可用。")" >&2
    fi

    local iface
    iface=$(ip -6 route show default 2>/dev/null | head -1 | awk '{print $5}')
    if [ -z "$iface" ]; then
        log "$(t "No default IPv6 route found." "未找到默认 IPv6 路由。")" >&2
        echo "none"; return 0
    fi
    log "$(t "Default IPv6 interface: $iface" "默认 IPv6 网卡: $iface")" >&2

    local ipv6_full ipv6_addr prefix_len
    # 地址选择策略：
    #   1. 优先选择静态（非 dynamic）的子网地址（排除 /128），按前缀降序排列
    #      /64 优先于 /48，因为 /48 通常是 ISP 的整体分配，/64 才是用户实际可用的子网
    #   2. 如果没有静态子网地址，回退到所有地址（含动态、含 /128）

    # 第一步：静态子网地址（不含 dynamic，不含 /128），按前缀降序——优先更具体的子网
    ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
        | grep "scope global" | grep -v "dynamic" \
        | awk '{print $2}' | grep -v '/128$' | sort -t'/' -k2 -rn | head -1)

    # 第二步：如果无静态子网，尝试所有静态地址（含 /128）
    if [ -z "$ipv6_full" ]; then
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" | grep -v "dynamic" \
            | awk '{print $2}' | sort -t'/' -k2 -rn | head -1)
    fi

    # 第三步：如果无静态地址，回退到动态地址
    if [ -z "$ipv6_full" ]; then
        log "$(t "No static IPv6 found, trying dynamic addresses..." "未找到静态 IPv6，尝试动态地址...")" >&2
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" \
            | awk '{print $2}' | grep -v '/128$' | sort -t'/' -k2 -rn | head -1)
    fi

    # 第四步：最终回退——任意全局地址
    if [ -z "$ipv6_full" ]; then
        ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null \
            | grep "scope global" \
            | awk '{print $2}' | sort -t'/' -k2 -rn | head -1)
    fi

    if [ -z "$ipv6_full" ]; then
        log "$(t "No global IPv6 address found." "未找到全局 IPv6 地址。")" >&2
        echo "none"; return 0
    fi

    ipv6_addr=$(echo "$ipv6_full" | cut -d'/' -f1)
    prefix_len=$(echo "$ipv6_full" | cut -d'/' -f2)
    log "$(t "IPv6 address: $ipv6_addr/$prefix_len" "IPv6 地址: $ipv6_addr/$prefix_len")" >&2

    # subnet 模式要求前缀 ≤ /64，且 ISP 必须真正允许子网内多个 IP 出站。
    # 有些 ISP 接口显示 /64 但做了严格 uRPF，只允许分配的单个 /128 出站。
    # /65-/127 前缀太小，/128 单地址，均使用 SNAT 模式。
    if [ "$prefix_len" -le 64 ]; then
        # 实际连通性验证：临时添加 addr+1，测试该地址能否独立出站
        local test_addr
        test_addr=$(ipv6_plus_one "$ipv6_addr")
        local subnet_ok=0
        if [ -n "$test_addr" ]; then
            ip addr add "$test_addr/$prefix_len" dev "$iface" 2>/dev/null && {
                sleep 2  # 等待 DAD 完成
                for _ep in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                    if curl -6 --interface "$test_addr" -s --max-time 8 "$_ep" >/dev/null 2>&1; then
                        subnet_ok=1; break
                    fi
                done
                ip addr del "$test_addr/$prefix_len" dev "$iface" 2>/dev/null
            }
        fi
        if [ "$subnet_ok" = "1" ]; then
            log "$(t "✓ Subnet mode confirmed (/$prefix_len), additional IPs are routable." "✓ 子网模式验证通过 (/$prefix_len)，子网内多 IP 可独立出站。")" >&2
            echo "$iface|$ipv6_addr|$prefix_len"
        else
            log "$(t "⚠ ISP blocks non-assigned source IPs (strict uRPF), falling back to SNAT mode." "⚠ ISP 严格过滤非分配源 IP（strict uRPF），回退到 SNAT 模式。")" >&2
            echo "$iface|$ipv6_addr|128"
        fi
    elif [ "$prefix_len" -eq 128 ]; then
        log "$(t "✓ Public IPv6 single address (/128) detected, SNAT mode." "✓ 检测到公网 IPv6 单地址 (/128)，使用 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128"
    else
        # /65-/127：前缀不足以划分子网，回退到 SNAT
        log "$(t "IPv6 prefix /$prefix_len is too small for subnet mode (need ≤/64), falling back to SNAT." "IPv6 前缀 /$prefix_len 不足以用于子网模式（需 ≤/64），回退到 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128"
    fi
}

enable_bbr() {
    log "$(t "Configuring kernel parameters (BBR + forwarding)..." "配置内核参数 (BBR + 转发)...")"
    # 多厂商兼容：DO/Hetzner/部分 VPS 默认 ip_forward=0、IPv6 forwarding=0，
    # 容器/VM 出网会失败；启用 forwarding 后 IPv6 RA 默认会被拒，accept_ra=2 才能保留默认路由。
    cat > /etc/sysctl.d/99-narwhalcloud.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
fs.inotify.max_user_instances=524288
fs.inotify.max_user_watches=1048576
fs.inotify.max_queued_events=65536
kernel.pid_max=4194304
fs.file-max=2097152
fs.nr_open=2097152
vm.swappiness=100
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.default.forwarding=1
net.ipv6.conf.all.accept_ra=2
net.ipv6.conf.default.accept_ra=2
net.ipv6.conf.all.use_tempaddr=0
net.ipv6.conf.default.use_tempaddr=0
EOF
    sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1 \
        && log "$(t "✓ Kernel parameters configured." "✓ 内核参数已配置。")" \
        || log "$(t "Kernel parameters will take effect after reboot." "内核参数将在重启后生效。")"
}

configure_journald() {
    log "$(t "Configuring systemd-journald limits..." "配置 systemd-journald 日志限制...")"
    cat > /etc/systemd/journald.conf <<EOF
[Journal]
SystemMaxUse=200M
RuntimeMaxUse=50M
SystemMaxFileSize=50M
RateLimitIntervalSec=10s
RateLimitBurst=1000
ForwardToSyslog=no
EOF
    systemctl restart systemd-journald
    log "$(t "✓ systemd-journald configured and restarted." "✓ systemd-journald 已配置并重启。")"
}

create_xfs_disk() {
    local disk=$1 size=$2 mount=$3 opts=$4
    if [ ! -f "$disk" ]; then
        log "$(t "Creating XFS disk $disk ($size)..." "创建 XFS 磁盘 $disk ($size)...")"
        # 确保完全预分配空间，避免稀疏文件导致的高负载下挂载挂起
        if ! fallocate -l "$size" "$disk" 2>/dev/null; then
            log "$(t "fallocate failed, using dd..." "fallocate 失败，改用 dd...")"
            local m_size
            m_size=$(echo "$size" | sed 's/[Gg]/*1024/;s/[Mm]//' | bc 2>/dev/null || echo "20480")
            dd if=/dev/zero of="$disk" bs=1M count="$m_size" status=progress
        fi
        mkfs.xfs -f "$disk"
    else
        log "$(t "$disk already exists, skipping." "$disk 已存在，跳过。")"
    fi
    mkdir -p "$mount"

    # 配置 udev 规则以开启 Direct IO，解决双重缓存导致的内存压力和挂载进程异常问题
    log "$(t "Configuring udev rules for Direct IO..." "配置 udev 规则以开启 Direct IO...")"
    cat > /etc/udev/rules.d/99-loop-directio.rules <<EOF
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="loop*", ATTR{loop/backing_file}=="$disk", ATTR{loop/direct_io}="1"
EOF
    udevadm control --reload-rules && udevadm trigger

    if ! mountpoint -q "$mount"; then
        log "$(t "Mounting $disk..." "正在挂载 $disk...")"
        mount -o "$opts" "$disk" "$mount"
        # 立即尝试对当前设备开启 Direct IO
        local loop_dev
        loop_dev=$(mount | grep "$mount" | awk '{print $1}')
        if [[ "$loop_dev" == /dev/loop* ]]; then
            echo 1 > "/sys/block/${loop_dev#/dev/}/loop/direct_io" 2>/dev/null || true
        fi
    fi
    grep -q "$disk" /etc/fstab || echo "$disk $mount xfs $opts 0 0" >> /etc/fstab
}

download_agent() {
    local arch=$1
    mkdir -p "$AGENT_BIN_DIR"

    # 调试模式：检查本地文件
    if [ -f "./runman-agent-linux-$arch" ]; then
        log "$(t "Found local agent binary: ./runman-agent-linux-$arch (debug mode)" "找到本地 agent 二进制: ./runman-agent-linux-$arch (调试模式)")"
        cp "./runman-agent-linux-$arch" "$AGENT_BINARY.new"
        chmod +x "$AGENT_BINARY.new"
        mv "$AGENT_BINARY.new" "$AGENT_BINARY"
        log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY (from local)" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY (来自本地)")"
        return 0
    fi

    # 生产模式：从远程下载
    log "$(t "Downloading NarwhalCloud Agent ($arch)..." "下载 NarwhalCloud Agent ($arch)...")"
    download_with_retry "$DOWNLOAD_BASE/runman-agent-linux-$arch" "$AGENT_BINARY.new"
    chmod +x "$AGENT_BINARY.new"
    mv "$AGENT_BINARY.new" "$AGENT_BINARY"
    log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY")"
}

# write_service_file
write_service_file() {
    local virt_type="${1:-podman}"
    mkdir -p "$AGENT_DATA_DIR"

    # Determine After dependencies based on virt type
    local after_target="network-online.target"
    if [ "$virt_type" = "podman" ]; then
        after_target="network-online.target podman.socket"
    fi

    cat > "/etc/systemd/system/$AGENT_SERVICE.service" <<EOF
[Unit]
Description=NarwhalCloud Agent
After=$after_target
Wants=network-online.target

[Service]
Type=simple
User=root
OOMScoreAdjust=-999
KillMode=process
WorkingDirectory=$AGENT_BIN_DIR
ExecStart=$AGENT_BINARY --config $AGENT_CONFIG_FILE
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# write_config_file - Generate the main configuration file
write_config_file() {
    local virt_type="${1:-podman}"
    local ipv6_mode="${2:-none}"
    local ipv6_subnet="${3:-}"
    local ipv6_addr="${4:-}"
    local ipv6_iface="${5:-}"
    local ndp_iface="${6:-}"
    local ndp_subnets="${7:-}"
    local ndp_network="${8:-}"
    local web_user="${9:-admin}"
    local web_pass="${10:-}"

    mkdir -p "$AGENT_CONFIG_DIR"

    cat > "$AGENT_CONFIG_FILE" <<EOF
{
    "db": "$AGENT_DB",
    "web": ":$AGENT_WEB_PORT",
    "virt_type": "$virt_type",
    "token": "",
    "monitor_nic": "",
    "monitor_disk": "/",
    "web_user": "$web_user",
    "web_pass_hash": "$web_pass",
    "host": "",
    "max_port_forward": 20,
    "ipv6_mode": "$ipv6_mode",
    "ipv6_subnet": "$ipv6_subnet",
    "ipv6_addr": "$ipv6_addr",
    "ipv6_iface": "$ipv6_iface",
    "ndp_iface": "$ndp_iface",
    "ndp_subnets": "$ndp_subnets",
    "ndp_network": "$ndp_network",
    "ipv6_wg_subnet": "$INCUS_WG_IPV6_SUBNET",
    "ipv6_backup_dir": "$INCUS_IPV6_BACKUP_DIR",
    "incus_banner_preset": "$INCUS_BANNER_PRESET",
    "incus_banner_text": "$INCUS_BANNER_TEXT",
    "incus_ipv6_alloc": $INCUS_IPV6_ALLOC,
    "incus_alpine_base": "$INCUS_ALPINE_BASE",
    "incus_image_mirror": "$INCUS_IMAGE_MIRROR",
    "incus_ipv6_only": $([ "${INCUS_IPV6_ONLY:-}" = "1" ] && echo true || echo false),
    "rfw_addr": "$RFW_API_ADDR"
}
EOF
    chmod 600 "$AGENT_CONFIG_FILE"
    log "$(t "✓ Configuration file written to $AGENT_CONFIG_FILE" "✓ 配置文件已写入 $AGENT_CONFIG_FILE")"
}

install_rfw() {
    local mode="${1:-0}" # 0: normal (prompt), 1: force (no prompt), 2: update-only (no prompt, only if exists)
    local ARCH
    ARCH=$(detect_arch)

    if [ "$mode" = "0" ]; then
        read -rp "$(t "Install rfw firewall? [Y/n]: " "是否安装 rfw 防火墙? [Y/n]: ")" _rfw
        [[ ! "${_rfw:-Y}" =~ ^[Yy]$ ]] && { log "$(t "Skipping rfw installation." "跳过 rfw 安装。")"; return 0; }
    elif [ "$mode" = "2" ]; then
        [ -f "$RFW_BIN_DIR/rfw" ] || return 0
    fi

    log "$(t "Installing/Updating rfw..." "正在安装/更新 rfw...")"
    mkdir -p "$RFW_BIN_DIR"

    # 调试模式：检查本地文件
    if [ -f "./rfw-$ARCH" ]; then
        log "$(t "Found local rfw binary: ./rfw-$ARCH (debug mode)" "找到本地 rfw 二进制: ./rfw-$ARCH (调试模式)")"
        cp "./rfw-$ARCH" "$RFW_BIN_DIR/rfw"
        chmod +x "$RFW_BIN_DIR/rfw"
    else
        # 生产模式：从远程下载 (即使已存在也更新)
        if download_with_retry "$RFW_BASE/rfw-$ARCH" "$RFW_BIN_DIR/rfw.new"; then
            chmod +x "$RFW_BIN_DIR/rfw.new"
            systemctl is-active --quiet rfw && systemctl stop rfw
            mv "$RFW_BIN_DIR/rfw.new" "$RFW_BIN_DIR/rfw"
        else
             log "$(t "Warning: rfw download failed." "警告: rfw 下载失败。")"
             [ -f "$RFW_BIN_DIR/rfw" ] || return 0
        fi
    fi

    if [ ! -f "/etc/systemd/system/rfw.service" ]; then
        # 过滤掉 lo 及虚拟网桥
        mapfile -t interfaces < <(ip -o link show | awk -F': ' '{print $2}' \
            | grep -v "^lo$" | grep -v "^narwhal-net$" | grep -v "^incusbr0$" \
            | grep -v "^podman" | grep -v "@")

        if [ ${#interfaces[@]} -eq 0 ]; then
            log "$(t "Error: no available WAN interfaces found." "错误: 未找到可用的 WAN 网卡。")"
            return 1
        elif [ ${#interfaces[@]} -eq 1 ]; then
            SEL_IFACE="${interfaces[0]}"
            log "$(t "Only one WAN interface found, using: $SEL_IFACE" "只找到一个 WAN 网卡，使用: $SEL_IFACE")"
        else
            echo "$(t "Available WAN interfaces:" "可用 WAN 网卡：")"
            for i in "${!interfaces[@]}"; do echo "  $((i+1)). ${interfaces[$i]}"; done
            while true; do
                read -rp "$(t "Select interface number (1-${#interfaces[@]}): " "请选择网卡编号 (1-${#interfaces[@]}): ")" choice
                [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#interfaces[@]}" ] && break
                echo "$(t "Invalid choice, please try again." "无效选择，请重新输入。")"
            done
            SEL_IFACE="${interfaces[$((choice-1))]}"
            log "$(t "Selected interface: $SEL_IFACE" "选择网卡: $SEL_IFACE")"
        fi

        cat > /etc/systemd/system/rfw.service <<EOF
[Unit]
Description=RFW eBPF Firewall
After=network.target

[Service]
Type=simple
User=root
Environment=RUST_LOG=info
WorkingDirectory=$RFW_BIN_DIR
ExecStart=$RFW_BIN_DIR/rfw --iface $SEL_IFACE --api-addr $RFW_API_ADDR
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        log "$(t "✓ rfw service file written. API listens on $RFW_API_ADDR (local only)." "✓ rfw 服务文件已写入，API 监听 $RFW_API_ADDR（仅本地）。")"
    fi
    
    start_service rfw

    # Wait for rfw API to become ready (up to 10s)
    _rfw_ready=0
    for _i in 1 2 3 4 5 6 7 8 9 10; do
        curl -sf "http://$RFW_API_ADDR/api/rules" >/dev/null 2>&1 && { _rfw_ready=1; break; }
        sleep 1
    done

    if [ "$_rfw_ready" = "1" ]; then
        # Check if rules already exist
        _current_rules=$(curl -sf "http://$RFW_API_ADDR/api/rules")
        if [ "$_current_rules" != "[]" ] && [ -n "$_current_rules" ]; then
            log "$(t "rfw rules already exist, skipping default rules." "rfw 规则已存在，跳过默认规则安装。")"
        else
            log "$(t "Installing default rfw rules..." "安装默认 rfw 规则...")"
            _geoip_countries='["CN"]'

            # Block inbound http/socks/fet from high-risk regions
            for _proto in http socks fet; do
                curl -sf -X POST "http://$RFW_API_ADDR/api/rules" \
                    -H "Content-Type: application/json" \
                    -d "{\"priority\":100,\"direction\":\"in\",\"protocol\":\"$_proto\",\"action\":\"block\",\"port_start\":0,\"enabled\":true,\"ip_type\":\"geoip\",\"countries\":$_geoip_countries}" \
                    >/dev/null
            done

            # Block outbound SMTP ports (anti-spam)
            for _port in 25 465 587; do
                curl -sf -X POST "http://$RFW_API_ADDR/api/rules" \
                    -H "Content-Type: application/json" \
                    -d "{\"priority\":100,\"direction\":\"out\",\"protocol\":\"tcp\",\"action\":\"block\",\"port_start\":$_port,\"enabled\":true,\"ip_type\":\"any\"}" \
                    >/dev/null
            done
            log "$(t "✓ Default rfw rules installed/verified." "✓ 默认 rfw 规则已安装/确认。")"
        fi
    else
        log "$(t "Warning: rfw API not ready, skipping default rules." "警告：rfw API 未就绪，跳过默认规则安装。")"
    fi

    log "$(t "✓ rfw firewall installed/updated." "✓ rfw 防火墙已安装/更新。")"
}

# ── VM Images Updater ────────────────────────────────────────────────────────

update_vm_images() {
    local IMGDIR="${VM_IMAGE_DIR:-/opt/vm-images}"
    local FORCE=0
    # 可通过参数指定要构建的发行版（预构建镜像下载失败时的回退），默认全部
    local TARGETS=("$@")
    [ ${#TARGETS[@]} -eq 0 ] && TARGETS=(debian alpine)

    log "$(t "VM image update started. Images dir: $IMGDIR" "VM 镜像更新开始。镜像目录: $IMGDIR")"

    require_tools() {
        for t in wget qemu-img mount umount losetup; do
            command -v "$t" &>/dev/null || die "Missing tool: $t"
        done
    }

    # Mount the root partition of a raw image, run a function with the mountpoint,
    # then unmount. Automatically finds the largest Linux filesystem partition.
    with_rootfs() {
        local img="$1" fn="$2"
        local mnt offset size best_size=0

        while read -r o s; do
            if [ "$s" -gt "$best_size" ]; then
                best_size="$s"; offset="$o"; size="$s"
            fi
        done < <(list_partitions "$img")

        mnt=$(mktemp -d)
        mount -o loop,offset="$offset",sizelimit="$size" "$img" "$mnt" 2>/dev/null \
            || { rmdir "$mnt"; die "Cannot mount rootfs of $img"; }

        "$fn" "$mnt"

        umount "$mnt" && rmdir "$mnt"
    }

    # ── Per-distro customization hooks ───────────────────────────────────────────
    # 与 CI 预构建镜像保持一致：软件包与 sshd root 登录配置直接烤入镜像，
    # Agent 的实例级 cloud-init 不再装包/改 sshd 配置

    bake_sshd_config() {
        local root="$1"
        mkdir -p "$root/etc/ssh/sshd_config.d"
        printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > "$root/etc/ssh/sshd_config.d/99-runman.conf"
        sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' "$root/etc/ssh/sshd_config" 2>/dev/null || true
        sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' "$root/etc/ssh/sshd_config" 2>/dev/null || true
    }

    customize_debian() {
        local root="$1"
        log "  Baking packages and sshd config (debian)..."
        mount -t proc proc "$root/proc" 2>/dev/null || true
        mount --bind /dev "$root/dev" 2>/dev/null || true
        mv "$root/etc/resolv.conf" "$root/etc/resolv.conf.bak" 2>/dev/null || true
        echo "nameserver 1.1.1.1" > "$root/etc/resolv.conf"
        # 阻止 chroot 内维护脚本启动服务
        printf '#!/bin/sh\nexit 101\n' > "$root/usr/sbin/policy-rc.d"
        chmod +x "$root/usr/sbin/policy-rc.d"
        chroot "$root" env DEBIAN_FRONTEND=noninteractive sh -c \
            'apt-get update && apt-get install -y curl e2fsprogs cloud-guest-utils openssh-server && apt-get clean && rm -rf /var/lib/apt/lists/*' \
            || log "  Warning: package bake failed, image may lack curl/e2fsprogs"
        chroot "$root" systemctl enable ssh >/dev/null 2>&1 || true
        rm -f "$root/usr/sbin/policy-rc.d" "$root/etc/resolv.conf"
        mv "$root/etc/resolv.conf.bak" "$root/etc/resolv.conf" 2>/dev/null || true
        bake_sshd_config "$root"
        umount "$root/dev" 2>/dev/null || true
        umount "$root/proc" 2>/dev/null || true
    }

    customize_alpine() {
        local root="$1"
        log "  Baking packages and sshd config (alpine)..."
        mv "$root/etc/resolv.conf" "$root/etc/resolv.conf.bak" 2>/dev/null || true
        echo "nameserver 1.1.1.1" > "$root/etc/resolv.conf"
        chroot "$root" /sbin/apk add --no-cache curl e2fsprogs openssh \
            || log "  Warning: package bake failed, image may lack curl/e2fsprogs"
        rm -f "$root/etc/resolv.conf"
        mv "$root/etc/resolv.conf.bak" "$root/etc/resolv.conf" 2>/dev/null || true
        bake_sshd_config "$root"
        ln -sf /etc/init.d/sshd "$root/etc/runlevels/default/sshd" 2>/dev/null || true
    }

    # Convert qcow2 to raw
    to_raw() {
        local src="$1" dst="$2"
        log "  Converting to raw..."
        qemu-img convert -f qcow2 -O raw "$src" "$dst"
    }

    # List all partitions in a raw image as "offset size" pairs (in bytes)
    list_partitions() {
        local img="$1"
        fdisk -l "$img" | awk -v img="$img" '
            $0 ~ "^"img {
                # fdisk may insert a boot flag "*" as $2 — shift fields accordingly
                if ($2 == "*") { start=$3; sectors=$5 }
                else            { start=$2; sectors=$4 }
                offset = start * 512
                size   = sectors * 512
                if (offset > 0 && size > 0)
                    print offset, size
            }
        '
    }

    # Try to mount a partition and run a probe function; unmount and return status
    try_mount() {
        local img="$1" offset="$2" size="$3" mnt="$4"
        mkdir -p "$mnt"
        mount -o loop,offset="$offset",sizelimit="$size",ro "$img" "$mnt" 2>/dev/null
    }

    # Extract kernel and initrd from a raw disk image.
    # Scans all partitions automatically to find vmlinuz/initrd.
    extract_kernel() {
        local raw="$1" outdir="$2"
        local mnt vmlinuz initrd found=0

        log "  Extracting kernel and initrd..."

        while read -r offset size; do
            mnt=$(mktemp -d)
            if ! try_mount "$raw" "$offset" "$size" "$mnt"; then
                rmdir "$mnt" 2>/dev/null || true
                continue
            fi

            vmlinuz=$(find "$mnt" "$mnt/boot" -maxdepth 1 -name "vmlinuz-*" 2>/dev/null | grep -v rescue | sort -V | tail -1)
            initrd=$(find  "$mnt" "$mnt/boot" -maxdepth 1 \( -name "initrd*" -o -name "initramfs-*.img" \) 2>/dev/null \
                     | grep -v rescue | sort -V | tail -1)

            if [ -n "$vmlinuz" ] && [ -n "$initrd" ]; then
                cp "$vmlinuz" "$outdir/vmlinuz"
                cp "$initrd"  "$outdir/initrd"
                found=1
            fi

            umount "$mnt" && rmdir "$mnt"
            [ "$found" -eq 1 ] && break
        done < <(list_partitions "$raw")

        [ "$found" -eq 1 ] || die "vmlinuz/initrd not found in any partition of $raw"
        log "  Kernel: $(basename "$vmlinuz")"
    }

    # ── Per-distro build functions ────────────────────────────────────────────────

    build_debian() {
        local dir="$IMGDIR/debian"
        mkdir -p "$dir"
        log "[Debian 13]"

        local base="https://cloud.debian.org/images/cloud/trixie/latest"
        local qcow2="$dir/debian-13-generic-amd64.qcow2"
        local sha_url="$base/SHA512SUMS"

        if [ -f "$qcow2" ] && [ "$FORCE" -eq 0 ]; then
            local remote_sha local_sha
            remote_sha=$(wget -qO- "$sha_url" | grep "debian-13-generic-amd64.qcow2" | awk '{print $1}')
            local_sha=$(sha512sum "$qcow2" 2>/dev/null | awk '{print $1}' || true)
            if [ "$remote_sha" = "$local_sha" ]; then
                log "  Already up-to-date"; return
            fi
        fi

        wget -q --show-progress "$base/debian-13-generic-amd64.qcow2" -O "$qcow2"
        to_raw "$qcow2" "$dir/current.raw"
        extract_kernel "$dir/current.raw" "$dir"
        with_rootfs "$dir/current.raw" customize_debian

        echo "root=/dev/vda1 rw console=ttyS0,115200" > "$dir/cmdline"
        log "  Done: $dir"
    }

    build_alpine() {
        local dir="$IMGDIR/alpine"
        mkdir -p "$dir"
        log "[Alpine Linux (nocloud cloud image)]"

        local cloud_base=""
        local latest_qcow2=""

        # Try to resolve from latest-stable releases/cloud
        if wget -q --spider "https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/cloud/" 2>/dev/null; then
            cloud_base="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/cloud"
            latest_qcow2=$(wget -qO- "$cloud_base/" 2>/dev/null \
                | grep -o 'nocloud_alpine-[0-9][^"]*-x86_64-bios-cloudinit-r[0-9]*\.qcow2' \
                | sort -uV | tail -1 || true)
        fi

        # Fallback to older stable versions if the newest releases directory does not contain 'cloud' yet
        if [ -z "$latest_qcow2" ]; then
            log "No cloud image found in latest-stable, searching previous v3.x releases..."
            local versions
            versions=$(wget -qO- "https://dl-cdn.alpinelinux.org/alpine/" 2>/dev/null \
                | grep -o 'href=["'\''\]\?v3\.[0-9]*/\?["'\''\]\?' \
                | sed -e 's/href=//' -e 's/["'\''/]//g' \
                | sort -t. -k2,2rn || true)

            for ver in $versions; do
                local test_url="https://dl-cdn.alpinelinux.org/alpine/$ver/releases/cloud"
                if wget -q --spider "$test_url/" 2>/dev/null; then
                    local qcow
                    qcow=$(wget -qO- "$test_url/" 2>/dev/null \
                        | grep -o 'nocloud_alpine-[0-9][^"]*-x86_64-bios-cloudinit-r[0-9]*\.qcow2' \
                        | sort -uV | tail -1 || true)
                    if [ -n "$qcow" ]; then
                        cloud_base="$test_url"
                        latest_qcow2="$qcow"
                        log "Found cloud image in $ver: $latest_qcow2"
                        break
                    fi
                fi
            done
        fi

        [ -n "$latest_qcow2" ] || die "Could not resolve Alpine cloud image filename"

        local latest_ver
        latest_ver=$(echo "$latest_qcow2" | grep -o '[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*')
        local current_ver
        current_ver=$(cat "$dir/version" 2>/dev/null || true)

        if [ "$latest_ver" = "$current_ver" ] && [ "$FORCE" -eq 0 ]; then
            log "  Already up-to-date ($latest_ver)"; return
        fi

        log "  Latest version: $latest_ver ($latest_qcow2)"
        local qcow2="$dir/$latest_qcow2"

        if [ -f "$qcow2" ] && [ "$FORCE" -eq 0 ]; then
            local remote_sha local_sha
            remote_sha=$(wget -qO- "$cloud_base/${latest_qcow2}.sha512" | awk '{print $1}')
            local_sha=$(sha512sum "$qcow2" 2>/dev/null | awk '{print $1}' || true)
            if [ "$remote_sha" = "$local_sha" ]; then
                log "  Already up-to-date (SHA512 matches)"; return
            fi
        fi

        wget -q --show-progress "$cloud_base/$latest_qcow2" -O "$qcow2"
        to_raw "$qcow2" "$dir/current.raw"

        # Alpine nocloud image has no partition table — raw ext4 filesystem on the device.
        # Mount directly and extract kernel/initrd from /boot.
        local mnt
        mnt=$(mktemp -d)
        mount /opt/vm-images/alpine/current.raw "$mnt"
        cp "$mnt/boot/vmlinuz-virt"   "$dir/vmlinuz"
        cp "$mnt/boot/initramfs-virt" "$dir/initrd"
        log "  Kernel: vmlinuz-virt"
        customize_alpine "$mnt"
        umount "$mnt" && rmdir "$mnt"

        # Root is the whole device (no partition table), identified by UUID
        local root_uuid
        root_uuid=$(blkid -s UUID -o value "$dir/current.raw")
        [ -n "$root_uuid" ] || die "Could not detect root UUID from Alpine image"

        echo "root=UUID=$root_uuid rw modules=sd-mod,usb-storage,ext4 console=ttyS0,115200" > "$dir/cmdline"
        echo "$latest_ver" > "$dir/version"
        log "  Done: $dir (Alpine $latest_ver, cloud-init built-in)"
    }

    require_tools

    for target in "${TARGETS[@]}"; do
        case "$target" in
            debian) build_debian ;;
            alpine) build_alpine ;;
        esac
    done

    log "$(t "VM image update finished." "VM 镜像更新完成。")"
}

# ── Prebuilt VM image download ───────────────────────────────────────────────
# 从 narwhal-cloud/images 的滚动 release 下载 CI 预构建的 cloudhv 镜像包，
# 避免安装时在线转换 qcow2 / 提取内核。失败的发行版记录在 VM_FETCH_FAILED，
# 由调用方回退到 update_vm_images 本地构建。
#
# vm-latest 是滚动 tag，同名资产每月被覆盖，因此本地记录远端资产的 ETag /
# Last-Modified 作为版本戳（.image-stamp），更新流程据此判断是否需要重新下载。

# 返回远端资产的版本戳（优先 ETag，其次 Last-Modified）；无法获取时返回非 0
remote_asset_stamp() {
    local headers stamp
    headers=$(curl -fsSLI --max-time 30 "$1" 2>/dev/null | tr -d '\r' | tr 'A-Z' 'a-z') || return 1
    stamp=$(printf '%s\n' "$headers" | grep '^etag:' | tail -n1 | sed 's/^etag:[[:space:]]*//') || true
    [ -z "$stamp" ] && stamp=$(printf '%s\n' "$headers" | grep '^last-modified:' | tail -n1 | sed 's/^last-modified:[[:space:]]*//') || true
    [ -n "$stamp" ] || return 1
    printf '%s\n' "$stamp"
}

# VM_FETCH_FAILED: 本次下载失败的发行版
# VM_FETCH_ABSENT: 下载失败且本地没有可用镜像的发行版（需要回退本地构建）
VM_FETCH_FAILED=()
VM_FETCH_ABSENT=()
fetch_vm_images() {
    local IMGDIR="${VM_IMAGE_DIR:-/opt/vm-images}"
    # 1: 镜像已存在时也检查远端是否有新版本（更新流程使用）
    local refresh="${VM_IMAGE_REFRESH:-0}"
    VM_FETCH_FAILED=()
    VM_FETCH_ABSENT=()

    local distro dir asset url tmp stamp_file remote_stamp present
    for distro in debian alpine; do
        dir="$IMGDIR/$distro"
        asset="cloudhv-${distro}-${ARCH}.tar.zst"
        url="$VM_IMAGES_BASE/$asset"
        stamp_file="$dir/.image-stamp"
        present=0
        [ -f "$dir/current.raw" ] && [ -f "$dir/vmlinuz" ] && [ -f "$dir/initrd" ] && [ -f "$dir/cmdline" ] && present=1

        remote_stamp=""
        if [ "$present" = "1" ]; then
            if [ "$refresh" != "1" ]; then
                log "$(t "VM image $distro already present, skipping." "VM 镜像 $distro 已存在，跳过。")"
                continue
            fi
            if [ "$FORCE_IMAGE_REFRESH" != "1" ]; then
                remote_stamp=$(remote_asset_stamp "$url") || remote_stamp=""
                if [ -z "$remote_stamp" ]; then
                    log "$(t "Warning: cannot query remote version for $asset, keeping existing image." "警告: 无法获取 $asset 的远端版本信息，保留现有镜像。")"
                    continue
                fi
                if [ -f "$stamp_file" ] && [ "$(cat "$stamp_file" 2>/dev/null)" = "$remote_stamp" ]; then
                    log "$(t "VM image $distro is up to date." "VM 镜像 $distro 已是最新。")"
                    continue
                fi
                log "$(t "Newer prebuilt VM image found for $distro, updating..." "发现 $distro 的新预构建 VM 镜像，开始更新...")"
            else
                log "$(t "Force refreshing VM image $distro..." "强制刷新 VM 镜像 $distro...")"
            fi
        fi
        [ -n "$remote_stamp" ] || remote_stamp=$(remote_asset_stamp "$url") || remote_stamp=""

        tmp=$(mktemp)
        log "$(t "Downloading prebuilt VM image: $asset" "下载预构建 VM 镜像: $asset")"
        # 解压到临时目录后整体替换，避免覆盖过程中 Agent 读到半个镜像
        if download_with_retry "$url" "$tmp" \
            && rm -rf "$dir.new" && mkdir -p "$dir.new" \
            && tar --zstd -xf "$tmp" -C "$dir.new"; then
            [ -n "$remote_stamp" ] && printf '%s\n' "$remote_stamp" > "$dir.new/.image-stamp"
            rm -rf "$dir.old"
            [ "$present" = "1" ] && mv "$dir" "$dir.old"
            mkdir -p "$IMGDIR"
            mv "$dir.new" "$dir"
            rm -rf "$dir.old"
            log "$(t "✓ VM image $distro installed." "✓ VM 镜像 $distro 安装完成。")"
        else
            rm -rf "$dir.new"
            VM_FETCH_FAILED+=("$distro")
            if [ "$present" = "1" ]; then
                log "$(t "Warning: prebuilt $distro image unavailable, keeping existing image." "警告: 预构建 $distro 镜像不可用，保留现有镜像。")"
            else
                log "$(t "Warning: prebuilt $distro image unavailable, will build locally." "警告: 预构建 $distro 镜像不可用，将回退本地构建。")"
                VM_FETCH_ABSENT+=("$distro")
            fi
        fi
        rm -f "$tmp"
    done

    [ ${#VM_FETCH_FAILED[@]} -eq 0 ]
}

# ── Prebuilt Incus image import ──────────────────────────────────────────────
# 导入预构建的 ready 镜像（别名与 Agent ensureReadyImage 一致），跳过首次创建
# 实例时的在线构建；下载失败时 Agent 会自动回退构建。
# INCUS_IMAGE_REFRESH=1 时对已存在的别名检查远端更新并重新导入。

incus_ready_alias() {
    case "$1" in
        debian) echo "debian/13/cloud/$ARCH/ready" ;;
        alpine) echo "alpine/3.23/cloud/$ARCH/ready" ;;
    esac
}

import_incus_images() {
    local refresh="${INCUS_IMAGE_REFRESH:-0}"
    local stamp_dir="$AGENT_DATA_DIR/incus-image-stamps"
    command -v incus &>/dev/null || return 0
    mkdir -p "$stamp_dir"

    local distro alias asset url tmp stamp_file remote_stamp present
    for distro in debian alpine; do
        alias=$(incus_ready_alias "$distro")
        asset="incus-${distro}-${ARCH}.tar.gz"
        # 支持通过 INCUS_IMAGE_MIRROR（本地静态服务/内网镜像）覆盖默认的 GitHub releases 基址
        local img_base="$VM_IMAGES_BASE"
        [ -n "$INCUS_IMAGE_MIRROR" ] && img_base="${INCUS_IMAGE_MIRROR%/}"
        url="$img_base/$asset"
        stamp_file="$stamp_dir/$distro-$ARCH"
        present=0
        incus image alias list --format csv 2>/dev/null | grep -q "^${alias}," && present=1

        remote_stamp=""
        if [ "$present" = "1" ]; then
            if [ "$refresh" != "1" ]; then
                log "$(t "Incus image $alias already exists, skipping." "Incus 镜像 $alias 已存在，跳过。")"
                continue
            fi
            if [ "$FORCE_IMAGE_REFRESH" != "1" ]; then
                remote_stamp=$(remote_asset_stamp "$url") || remote_stamp=""
                if [ -z "$remote_stamp" ]; then
                    log "$(t "Warning: cannot query remote version for $asset, keeping existing image." "警告: 无法获取 $asset 的远端版本信息，保留现有镜像。")"
                    continue
                fi
                if [ -f "$stamp_file" ] && [ "$(cat "$stamp_file" 2>/dev/null)" = "$remote_stamp" ]; then
                    log "$(t "Incus image $alias is up to date." "Incus 镜像 $alias 已是最新。")"
                    continue
                fi
                log "$(t "Newer prebuilt Incus image found for $distro, updating..." "发现 $distro 的新预构建 Incus 镜像，开始更新...")"
            else
                log "$(t "Force refreshing Incus image $alias..." "强制刷新 Incus 镜像 $alias...")"
            fi
        fi
        [ -n "$remote_stamp" ] || remote_stamp=$(remote_asset_stamp "$url") || remote_stamp=""

        tmp=$(mktemp)
        log "$(t "Downloading prebuilt Incus image: $asset" "下载预构建 Incus 镜像: $asset")"
        # 先下载成功再删除旧镜像，避免下载失败后本地无镜像可用
        if download_with_retry "$url" "$tmp"; then
            [ "$present" = "1" ] && incus image delete "$alias" >/dev/null 2>&1 || true
            if incus image import "$tmp" --alias "$alias"; then
                [ -n "$remote_stamp" ] && printf '%s\n' "$remote_stamp" > "$stamp_file"
                log "$(t "✓ Incus image imported: $alias" "✓ Incus 镜像已导入: $alias")"
            else
                rm -f "$stamp_file"
                log "$(t "Warning: Incus image import failed, agent will build it on first instance creation." "警告: Incus 镜像导入失败，Agent 将在首次创建实例时自动构建。")"
            fi
        else
            log "$(t "Warning: prebuilt Incus image unavailable, agent will build it on first instance creation." "警告: 预构建 Incus 镜像不可用，Agent 将在首次创建实例时自动构建。")"
        fi
        rm -f "$tmp"
    done
}

# 从本地目录直接离线导入预构建 incus 镜像（无需任何网络）。
# 目录内需包含 incus-<distro>-<arch>.tar.gz（文件名与远程资产一致）。
import_local_incus_images() {
    local dir="$INCUS_LOCAL_IMAGE_DIR"
    [ -d "$dir" ] || { log "$(t "Local image dir not found: $dir" "本地镜像目录不存在: $dir")"; return 1; }
    command -v incus >/dev/null 2>&1 || return 0
    mkdir -p "$AGENT_DATA_DIR/incus-image-stamps"
    for distro in debian alpine; do
        local asset="incus-${distro}-${ARCH}.tar.gz"
        local f="$dir/$asset"
        [ -f "$f" ] || { log "$(t "Skip $asset (not in local dir)" "跳过 $asset（本地目录不存在）")"; continue; }
        local alias; alias=$(incus_ready_alias "$distro")
        incus image delete "$alias" >/dev/null 2>&1 || true
        if incus image import "$f" --alias "$alias"; then
            log "$(t "Imported local incus image: $alias" "已导入本地 incus 镜像: $alias")"
        else
            log "$(t "Warning: failed to import local image $asset" "警告: 本地镜像 $asset 导入失败")"
        fi
    done
}

# 导入定制 alpine 基础镜像（如 podcctv/alpine-base）。
# 参数可为本地 tar.gz 路径（导入为别名 podcctv/alpine-base）或已存在的 incus 镜像别名。
import_custom_alpine_base() {
    [ -n "$INCUS_ALPINE_BASE" ] || return 0
    command -v incus >/dev/null 2>&1 || return 0
    if [ -f "$INCUS_ALPINE_BASE" ]; then
        local alias="podcctv/alpine-base"
        incus image delete "$alias" >/dev/null 2>&1 || true
        if incus image import "$INCUS_ALPINE_BASE" --alias "$alias"; then
            log "$(t "Imported custom alpine base as $alias" "已导入定制 alpine 基础镜像为 $alias")"
            INCUS_ALPINE_BASE="$alias"
        else
            log "$(t "Warning: failed to import custom alpine base" "警告: 定制 alpine 基础镜像导入失败")"
        fi
    else
        if incus image alias list --format csv 2>/dev/null | cut -d, -f1 | grep -qx "$INCUS_ALPINE_BASE" \
           || incus image list --format csv 2>/dev/null | grep -q "$INCUS_ALPINE_BASE"; then
            log "$(t "Using existing alpine base alias: $INCUS_ALPINE_BASE" "使用已存在的 alpine 基础镜像别名: $INCUS_ALPINE_BASE")"
        else
            log "$(t "Warning: alpine base alias not found: $INCUS_ALPINE_BASE" "警告: 未找到 alpine 基础镜像别名: $INCUS_ALPINE_BASE")"
        fi
    fi
}

# 从私有 simplestreams 镜像服务器导入镜像（fork 默认源）。
# 服务器以 LXD/Incus 原生格式（lxd.tar.xz + rootfs.squashfs）发布，
# 直接按 streams/v1/images.json 中的路径下载并 incus image import，
# 不依赖 streams/v1/index.json（部分自建服务器未提供，incus remote add 会失败）。
# 遍历 alpine / debian，服务器提供哪些就导入哪些（fork 服务器当前仅 alpine/amd64）。
# 返回 0 表示至少成功导入一个镜像（调用方据此决定其余发行版是否回退 GitHub）。
import_incus_images_from_mirror() {
    [ -n "$INCUS_IMAGE_MIRROR" ] || return 1
    [ "$ARCH" = "amd64" ] || { log "$(t "Mirror only provides amd64 images; skipping." "镜像服务器仅提供 amd64 镜像，跳过。")"; return 1; }
    command -v incus >/dev/null 2>&1 || return 1
    command -v jq    >/dev/null 2>&1 || { log "$(t "jq not available; skipping mirror import." "jq 不可用，跳过镜像导入。")"; return 1; }

    local base="${INCUS_IMAGE_MIRROR%/}"
    local meta
    meta=$(curl -fsSL --max-time 30 "$base/streams/v1/images.json" 2>/dev/null) || {
        log "$(t "Cannot reach mirror metadata: $base" "无法访问镜像元数据: $base")"; return 1
    }

    local ok=0 distro lxd_path rootfs_path tmpd lxd_file rootfs_file local_alias
    for distro in alpine debian; do
        lxd_path=$(printf '%s\n' "$meta" | jq -r --arg d "$distro" '.products | to_entries[]? | select(.key | startswith($d)) | .value.versions[]?.items["lxd.tar.xz"].path // empty' 2>/dev/null | head -1)
        [ -n "$lxd_path" ] || continue
        rootfs_path=$(printf '%s\n' "$meta" | jq -r --arg d "$distro" '.products | to_entries[]? | select(.key | startswith($d)) | .value.versions[]?.items["rootfs.squashfs"].path // empty' 2>/dev/null | head -1)
        [ -n "$rootfs_path" ] || continue

        tmpd=$(mktemp -d)
        lxd_file="$tmpd/lxd.tar.xz"; rootfs_file="$tmpd/rootfs.squashfs"
        if download_with_retry "$base/$lxd_path" "$lxd_file" && download_with_retry "$base/$rootfs_path" "$rootfs_file"; then
            if [ "$distro" = "alpine" ]; then local_alias="alpine/3.23/cloud/$ARCH/ready"; else local_alias="debian/13/cloud/$ARCH/ready"; fi
            incus image delete "$local_alias" >/dev/null 2>&1 || true
            if incus image import "$lxd_file" "$rootfs_file" --alias "$local_alias"; then
                log "$(t "✓ Imported $distro from mirror as $local_alias" "✓ 已从镜像服务器导入 $distro 为 $local_alias")"
                ok=1
            else
                log "$(t "Warning: incus image import from mirror failed for $distro" "警告: 从镜像服务器导入 $distro 失败")"
            fi
        else
            log "$(t "Warning: failed to download $distro image from mirror" "警告: 从镜像服务器下载 $distro 镜像失败")"
        fi
        rm -rf "$tmpd"
    done
    [ "$ok" = "1" ] || { log "$(t "Mirror provided no importable images" "镜像服务器未提供可导入的镜像")"; return 1; }
    return 0
}

# 交互式选择 SSH 登录横幅（欢迎页）预设；已通过参数/环境变量指定时跳过。
prompt_banner() {
    [ "$INCUS_BANNER_PRESET" != "none" ] && return 0
    read -rp "$(t "Configure SSH login banner? [1=none 2=default 3=project 4=custom]: " "配置 SSH 登录横幅? [1=无 2=默认 3=项目模板 4=自定义]: ")" _b
    case "${_b:-1}" in
        2) INCUS_BANNER_PRESET="default" ;;
        3) INCUS_BANNER_PRESET="project" ;;
        4) INCUS_BANNER_PRESET="custom"; read -rp "$(t "Enter banner text: " "请输入横幅文本: ")" INCUS_BANNER_TEXT ;;
        *) INCUS_BANNER_PRESET="none" ;;
    esac
}

# 从已存在的配置文件读取虚拟化类型（更新流程判断需要刷新哪类镜像）
detect_installed_virt_type() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 0
    grep -o '"virt_type"[[:space:]]*:[[:space:]]*"[^"]*"' "$AGENT_CONFIG_FILE" 2>/dev/null \
        | head -n1 | sed 's/.*:[[:space:]]*"//; s/"$//' || true
}

# ── Update flow ───────────────────────────────────────────────────────────────

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ]; then
    log "$(t "Existing NarwhalCloud Agent detected, updating..." "检测到已安装的 NarwhalCloud Agent，执行更新流程...")"

    # 配置迁移：为旧配置补齐新增字段（保留已有值，仅补充缺省），避免升级后新功能无法生效
    if command -v jq >/dev/null 2>&1 && [ -f "$AGENT_CONFIG_FILE" ]; then
        jq '. + {
               incus_banner_preset: (.incus_banner_preset // "none"),
               incus_banner_text:   (.incus_banner_text   // ""),
               incus_ipv6_alloc:    (.incus_ipv6_alloc    // 1),
               incus_alpine_base:   (.incus_alpine_base   // ""),
               incus_image_mirror:  (.incus_image_mirror  // ""),
               incus_ipv6_only:     (.incus_ipv6_only     // false),
               ipv6_wg_subnet:      (.ipv6_wg_subnet      // ""),
               ipv6_backup_dir:     (.ipv6_backup_dir     // "/var/lib/narwhal-agent/backups")
             }' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp" \
            && mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE" \
            && chmod 600 "$AGENT_CONFIG_FILE"
    fi

    ARCH=$(detect_arch)
    if command -v podman &>/dev/null; then
        check_podman_version
    fi

    # 重新应用 sysctl，确保旧安装升级后也获得最新内核参数（forwarding/accept_ra 等）
    enable_bbr
    configure_journald

    download_agent "$ARCH"

    # Update netavark
    if [ -f "/usr/libexec/podman/netavark" ]; then
        if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark.new"; then
            chmod +x /usr/libexec/podman/netavark.new
            mv /usr/libexec/podman/netavark.new /usr/libexec/podman/netavark
            log "$(t "✓ Custom netavark updated." "✓ 自定义 netavark 已更新。")"
        fi
    fi

    systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null && systemctl restart "$AGENT_SERVICE"
    log "$(t "✓ NarwhalCloud Agent updated." "✓ NarwhalCloud Agent 已更新。")"

    # Refresh prebuilt base images for VM-style virtualization.
    # 已运行的实例使用的是镜像副本，替换基础镜像只影响之后新建的实例。
    VIRT_TYPE=$(detect_installed_virt_type)
    case "$VIRT_TYPE" in
        cloudhv)
            log "$(t "Checking prebuilt VM images for updates..." "检查预构建 VM 镜像更新...")"
            VM_IMAGE_REFRESH=1
            if ! fetch_vm_images; then
                if [ ${#VM_FETCH_ABSENT[@]} -gt 0 ]; then
                    log "$(t "Falling back to local image build for: ${VM_FETCH_ABSENT[*]}" "回退到本地构建镜像: ${VM_FETCH_ABSENT[*]}")"
                    update_vm_images "${VM_FETCH_ABSENT[@]}"
                fi
            fi
            ;;
        incus)
            log "$(t "Checking prebuilt Incus images for updates..." "检查预构建 Incus 镜像更新...")"
            if [ -n "$INCUS_LOCAL_IMAGE_DIR" ]; then
                import_local_incus_images || true
            fi
            if [ -n "$INCUS_ALPINE_BASE" ]; then
                import_custom_alpine_base
            fi
            INCUS_IMAGE_REFRESH=1
            _nc_mirror_ok=0
            if [ -n "$INCUS_IMAGE_MIRROR" ] && import_incus_images_from_mirror; then
                _nc_mirror_ok=1
            else
                log "$(t "Mirror import failed on update; falling back to default source." "更新时镜像导入失败；回退到默认源。")"
            fi
            if [ "$_nc_mirror_ok" = "1" ]; then
                _nc_saved_mirror="$INCUS_IMAGE_MIRROR"
                INCUS_IMAGE_MIRROR="" import_incus_images
                INCUS_IMAGE_MIRROR="$_nc_saved_mirror"
            else
                import_incus_images
            fi
            ;;
    esac

    # Update rfw if binary exists or force install requested
    if [ "$INSTALL_RFW_FORCE" = "1" ]; then
        install_rfw 1
    else
        install_rfw 2
    fi

    IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
    echo ""
    log "========================================"
    log "$(t "✓ Update complete!" "✓ 更新完成！")"
    log "$(t "IP: $IP" "IP: $IP")"
    log "========================================"
    exit 0
fi

# ── Fresh install ─────────────────────────────────────────────────────────────

log "$(t "Starting fresh installation..." "开始全新安装流程...")"


# ── Virtualization type selection ──────────────────────────────────────────────

printf "$(t "Select virtualization type:" "选择虚拟化类型：")\n"
printf "  1) Podman container ($(t "recommended" "推荐"))\n"
printf "  2) cloud-hypervisor VM ($(t "experimental, requires /dev/kvm" "实验性，需要 /dev/kvm"))\n"
printf "  3) Incus (LXC) ($(t "experimental" "实验性"))\n"
log "$(t "WARNING: Types 2 and 3 are currently experimental and may not be stable." "警告：选项 2 和 3 目前处于实验阶段，稳定性可能不足。")"
read -rp "> " _virt_choice
case "${_virt_choice}" in
    2)
        VIRT_TYPE="cloudhv"
        # 检查 KVM 支持
        if [ ! -e "/dev/kvm" ]; then
            log "$(t "ERROR: /dev/kvm not found. KVM is required for cloud-hypervisor." "错误: 未找到 /dev/kvm。cloud-hypervisor 需要 KVM 支持。")"
            log "$(t "Please enable KVM in BIOS or use nested virtualization if running in a VM." "请在 BIOS 中启用 KVM，或在虚拟机中启用嵌套虚拟化。")"
            exit 1
        fi
        log "$(t "✓ KVM support detected." "✓ 检测到 KVM 支持。")"
        ;;
    3)
        VIRT_TYPE="incus"
        ;;
    *) VIRT_TYPE="podman" ;;
esac
log "$(t "Selected virtualization type: $VIRT_TYPE" "选择的虚拟化类型: $VIRT_TYPE")"

# ── Installation ───────────────────────────────────────────────────────────────

install_packages "$VIRT_TYPE"
if [ "$VIRT_TYPE" = "podman" ]; then
    check_podman_version
fi
enable_bbr
configure_journald
start_service chrony
if [ "$VIRT_TYPE" = "podman" ]; then
    start_service lxcfs
fi
if [ "$VIRT_TYPE" = "incus" ]; then
    # 0. 加载必要的内核模块 (security.ipv6_filtering 需要)
    log "$(t "Loading br_netfilter kernel module..." "加载 br_netfilter 内核模块...")"
    modprobe br_netfilter || true
    echo "br_netfilter" > /etc/modules-load.d/runman-incus.conf

    start_service incus
    # 等待 Socket 就绪
    sleep 2
fi

cat > /etc/systemd/zram-generator.conf <<'EOF'
[zram0]
compression-algorithm=lz4
zram-size=ram/2
fs-type=swap
swap-priority=100
EOF
start_service systemd-zram-setup@zram0.service

ARCH=$(detect_arch)
log "$(t "Architecture: $(uname -m) ($ARCH)" "架构: $(uname -m) ($ARCH)")"

if [ "$VIRT_TYPE" = "podman" ]; then
    start_service podman.socket
fi

# XFS data disk (Podman only - for container storage)
if [ "$VIRT_TYPE" = "podman" ]; then
    if [ -f "/xfs_disk.img" ]; then
        log "$(t "Data disk already exists, skipping." "数据盘已存在，跳过。")"
        mkdir -p /data
        # 启用 noatime 减少元数据压力，增加稳定性
        mountpoint -q /data || mount -o defaults,pquota,loop,noatime /xfs_disk.img /data
        # 确保对已存在的镜像也应用配置规则
        cat > /etc/udev/rules.d/99-loop-directio.rules <<EOF
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="loop*", ATTR{loop/backing_file}=="/xfs_disk.img", ATTR{loop/direct_io}="1"
EOF
        udevadm control --reload-rules && udevadm trigger
        _loop_dev=$(mount | grep "/data" | awk '{print $1}')
        if [[ "$_loop_dev" == /dev/loop* ]]; then
            echo 1 > "/sys/block/${_loop_dev#/dev/}/loop/direct_io" 2>/dev/null || true
        fi
    else
        read -rp "$(t "Enter data disk size (e.g. 20G): " "请输入数据盘大小 (例如 20G): ")" xfs_size
        xfs_size=${xfs_size:-20G}
        # 增加 noatime 优化性能和稳定性
        create_xfs_disk "/xfs_disk.img" "$xfs_size" "/data" "defaults,pquota,loop,noatime"
    fi
fi

if [ "$VIRT_TYPE" = "podman" ]; then
    cat > /etc/containers/storage.conf <<'EOF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/data/containers/storage"
EOF
    mkdir -p /data/containers/storage
fi

configure_host_ipv6_routing() {
    local iface="$1"
    local addr="$2"

    # 禁止 SLAAC 自动配置，防止内核生成额外的临时地址
    sysctl -w "net.ipv6.conf.$iface.autoconf=0" > /dev/null
    echo "net.ipv6.conf.$iface.autoconf=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    # 不处理 RA 中的前缀信息（彻底阻止 SLAAC 动态地址），但保留默认路由
    sysctl -w "net.ipv6.conf.$iface.accept_ra_pinfo=0" > /dev/null
    echo "net.ipv6.conf.$iface.accept_ra_pinfo=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    # forwarding=1 后必须 accept_ra=2 才能继续接收 RA 默认路由
    sysctl -w "net.ipv6.conf.$iface.accept_ra=2" > /dev/null
    echo "net.ipv6.conf.$iface.accept_ra=2" >> /etc/sysctl.d/99-narwhalcloud.conf

    # 将宿主机地址改为 /128，消除 /64 连接路由，避免 Podman 容器子网冲突
    if [ -n "$addr" ]; then
        local current_full
        current_full=$(ip -6 addr show dev "$iface" scope global 2>/dev/null \
            | awk '{print $2}' | grep -F "$addr/" | head -1)
        if [ -n "$current_full" ] && [ "$(echo "$current_full" | cut -d'/' -f2)" != "128" ]; then
            ip addr del "$current_full" dev "$iface" 2>/dev/null || true
            ip addr add "$addr/128" dev "$iface" 2>/dev/null || true
        fi
        # 移除 SLAAC/RA 动态生成的全局地址（如 EUI-64 /64），它们会产生 /64 连接路由
        ip -6 addr flush dev "$iface" scope global dynamic 2>/dev/null || true
        # 持久化到 /etc/network/interfaces
        if grep -q "iface $iface inet6 static" /etc/network/interfaces 2>/dev/null; then
            sed -i "/iface $iface inet6 static/,/^[^ ]/ s|address .*|address $addr/128|" /etc/network/interfaces
        else
            printf '\niface %s inet6 static\n    address %s/128\n' "$iface" "$addr" >> /etc/network/interfaces
        fi
    fi

    log "$(t "✓ Host IPv6 set to /128, SLAAC disabled, RA route preserved." "✓ 宿主机 IPv6 已设为 /128，SLAAC 已禁用，RA 默认路由保留。")"
}

# ── IPv6 detection ────────────────────────────────────────────────────────────

# 初始化 IPv6 模式变量
# IPV6_CONFIG: 检测原始结果，"none" 或 "iface|addr|prefix"
# IPV6_MODE:   最终运行模式，"none" / "snat" / "subnet"（写入 config.json）
# 保存用户通过环境变量显式指定的值（如 IPV6_MODE=subnet bash install.sh）
_USER_IPV6_MODE="${IPV6_MODE:-}"
IPV6_MODE="none"

# ── 在任何 IPv6 / 网卡修改之前，先做“变更前”完整备份（供卸载/回滚恢复）──
if [ "$_USER_IPV6_MODE" != "none" ]; then
    install_ipv6_rollback_helper 2>/dev/null || true
    if b=$(backup_ipv6_config); then
        log "$(t "IPv6 config backed up (pre-change) to $b" "IPv6 配置已备份（变更前）至 $b")"
    fi
fi

IPV6_CONFIG="none"

if [ "$_USER_IPV6_MODE" = "none" ]; then
    log "$(t "IPv6 mode pre-set to 'none', skipping detection." "IPv6 模式已预设为 'none'，跳过 IPv6 检测。")"
elif [ "$_USER_IPV6_MODE" = "snat" ]; then
    log "$(t "IPv6 mode pre-set to 'snat', skipping detection." "IPv6 模式已预设为 'snat'，跳过 IPv6 检测。")"
elif [ -n "$_USER_IPV6_MODE" ] && [ -n "${IPV6_ADDR:-}" ] && [ -n "${IPV6_SUBNET:-}" ]; then
    # 用户提供了完整的自定义配置（模式 + 地址 + 子网），跳过检测
    log "$(t "Custom IPv6 config provided (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET), skipping detection." \
        "已提供自定义 IPv6 配置 (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET)，跳过检测。")"
elif [ -n "$_USER_IPV6_MODE" ]; then
    log "$(t "IPv6 mode pre-set to '$_USER_IPV6_MODE', auto-detecting IPv6..." "IPv6 模式已预设为 '$_USER_IPV6_MODE'，自动检测 IPv6...")"
    IPV6_CONFIG=$(detect_and_configure_ipv6)
else
    read -rp "$(t "Enable public IPv6 detection? [Y/n]: " "是否进行公网 IPv6 检测? [Y/n]: ")" _ipv6
    if [[ "${_ipv6:-Y}" =~ ^[Yy]$ ]]; then
        IPV6_CONFIG=$(detect_and_configure_ipv6)
        if [ "$IPV6_CONFIG" = "none" ]; then
            read -rp "$(t "No IPv6 connectivity detected. Continue without IPv6? [Y/n]: " "未检测到 IPv6 连通性，是否不配置 IPv6 继续? [Y/n]: ")" _fb
            [[ "${_fb:-Y}" =~ ^[Nn]$ ]] && { log "$(t "Aborted." "已退出。")"; exit 1; }
        fi
    fi
fi

# ── IPv6 变量规范化（合并检测结果与用户 env var，统一供后续虚拟化分支使用）────────
# 优先使用用户通过 env var 显式提供的值，检测结果作为回退
IPV6_ADDR="${IPV6_ADDR:-}"
IPV6_SUBNET="${IPV6_SUBNET:-}"
IPV6_IFACE="${IPV6_IFACE:-}"
IPV6_PREFIX=""

if [ "$IPV6_CONFIG" != "none" ]; then
    # 从检测结果填充（env var 已有值时不覆盖）
    [ -z "$IPV6_IFACE" ] && IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
    [ -z "$IPV6_ADDR"  ] && IPV6_ADDR=$(echo  "$IPV6_CONFIG" | cut -d'|' -f2)
    IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)
fi

# 从 IPV6_SUBNET 提取前缀长度（当检测未运行或用户自定义子网时）
if [ -z "$IPV6_PREFIX" ] && [ -n "$IPV6_SUBNET" ]; then
    IPV6_PREFIX=$(echo "$IPV6_SUBNET" | cut -d'/' -f2)
fi

# IPV6_IFACE 未知时，从默认路由自动推断
if [ -z "$IPV6_IFACE" ] && [ -n "$IPV6_ADDR" ]; then
    IPV6_IFACE=$(ip -6 route show default 2>/dev/null | head -1 | awk '{print $5}')
fi

# 决定最终 IPv6 模式（用户显式指定优先，否则按前缀自动判断）
if [ -n "$_USER_IPV6_MODE" ]; then
    IPV6_MODE="$_USER_IPV6_MODE"
elif [ -n "$IPV6_PREFIX" ] && [ "$IPV6_PREFIX" -le 64 ]; then
    IPV6_MODE="subnet"
    # 未提供 IPV6_SUBNET 时，从 addr/prefix 计算
    if [ -z "$IPV6_SUBNET" ]; then
        IPV6_SUBNET=$(python3 -c "
import ipaddress
net = ipaddress.IPv6Network('$IPV6_ADDR/$IPV6_PREFIX', strict=False)
print(str(net))" 2>/dev/null)
    fi
elif [ -n "$IPV6_PREFIX" ]; then
    IPV6_MODE="snat"
fi
# IPV6_PREFIX 为空（无任何 IPv6）时 IPV6_MODE 保持 "none"

# 纯 IPv6 容器要求已具备 IPv6（subnet/snat），否则无法创建仅 IPv6 的实例
if [ "${INCUS_IPV6_ONLY:-}" = "1" ] && [ "$IPV6_MODE" = "none" ]; then
    die "$(t "IPv6-only containers require an IPv6 subnet (IPV6_MODE=subnet/snat); current mode is 'none'." \
        "纯 IPv6 容器需要可用的 IPv6 子网（IPV6_MODE=subnet/snat）；当前模式为 'none'。")"
fi

# 健壮性校验：subnet 模式必须有可用子网信息，否则降级
if [ "$IPV6_MODE" = "subnet" ] && [ -z "$IPV6_SUBNET" ]; then
    if [ -n "$IPV6_ADDR" ]; then
        log "$(t "Warning: subnet mode requested but no subnet info detected, falling back to SNAT." \
            "警告: 请求 subnet 模式但未检测到可用子网信息，降级到 SNAT。")"
        IPV6_MODE="snat"
    else
        log "$(t "Warning: subnet mode requested but no IPv6 detected, falling back to 'none'." \
            "警告: 请求 subnet 模式但未检测到 IPv6，降级到 'none'。")"
        IPV6_MODE="none"
    fi
fi

[ -n "$IPV6_ADDR" ] && log "$(t "IPv6: mode=$IPV6_MODE addr=$IPV6_ADDR prefix=${IPV6_PREFIX:--} iface=${IPV6_IFACE:--}" \
    "IPv6: 模式=$IPV6_MODE 地址=$IPV6_ADDR 前缀=${IPV6_PREFIX:--} 网卡=${IPV6_IFACE:--}")"

# ── Network setup (conditional on virtualization type) ────────────────────────

if [ "$VIRT_TYPE" = "cloudhv" ] || [ "$VIRT_TYPE" = "incus" ]; then
    # IPv6 变量已由规范化块统一解析，此处直接使用
    log "$(t "Setting up $VIRT_TYPE network..." "配置 $VIRT_TYPE 网络...")"

    if [ "$VIRT_TYPE" = "cloudhv" ]; then
        mkdir -p /opt/vm-images/instances /run/cloud-hypervisor
    fi

    if [ "$IPV6_MODE" = "none" ]; then
        log "$(t "No public IPv6, IPv6 will not be configured." "无公网 IPv6，将不配置 IPv6。")"
    elif [ "$IPV6_MODE" = "subnet" ]; then
        if [ -n "$IPV6_IFACE" ]; then
            configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"
        fi
        log "$(t "✓ IPv6 subnet mode: $IPV6_SUBNET, VMs will get independent addresses." "✓ IPv6 子网模式: $IPV6_SUBNET，VM 将获得独立地址。")"
    elif [ "$IPV6_MODE" = "snat" ]; then
        log "$(t "✓ IPv6 SNAT mode: VMs share host IPv6 via masquerade." "✓ IPv6 SNAT 模式：VM 通过宿主机 MASQUERADE 共享 IPv6。")"
    fi

    # Bridge creation will be handled by the agent's ensureBridge() on startup
    log "$(t "✓ $VIRT_TYPE network configured." "✓ $VIRT_TYPE 网络配置完成。")"

    if [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ] && [ -n "$IPV6_IFACE" ]; then
        log "$(t "✓ NDP responder will be enabled for IPv6 subnet mode." "✓ NDP 响应器将在 IPv6 子网模式下启用。")"
    fi
elif [ "$VIRT_TYPE" = "podman" ]; then
    # 先安装自定义 netavark（支持 snat_ipv6 选项），再操作网络
    if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark"; then
        chmod +x /usr/libexec/podman/netavark
        log "$(t "✓ Custom netavark installed." "✓ 自定义 netavark 已安装。")"
    else
        log "$(t "Warning: netavark download failed, using system default." "警告: netavark 下载失败，使用系统默认版本。")"
    fi

if podman network exists "$PODMAN_NETWORK" 2>/dev/null; then
    log "$(t "Podman network $PODMAN_NETWORK already exists, skipping." "Podman 网络 $PODMAN_NETWORK 已存在，跳过创建。")"
elif [ "$IPV6_MODE" = "none" ]; then
    log "$(t "Creating Podman network (IPv4 only)..." "创建 Podman 网络（仅 IPv4）...")"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (IPv4 only, no IPv6)." "✓ Podman 网络创建完成（仅 IPv4，无 IPv6）。")"
elif [ "$IPV6_MODE" = "snat" ]; then
    log "$(t "Creating Podman network (ULA IPv6, SNAT)..." "创建 Podman 网络（ULA IPv6，SNAT 模式）...")"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        --ipv6 \
        --subnet=fd91:cafe:cafe:10::/64 \
        --gateway=fd91:cafe:cafe:10::1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (ULA IPv6, SNAT via host address)." "✓ Podman 网络创建完成（ULA IPv6，通过宿主机地址 SNAT）。")"
elif [ "$IPV6_MODE" = "subnet" ]; then
    # 计算容器 /112 子网：在宿主机前缀内偏移固定量，避免与 EUI-64/隐私地址冲突
    read -r CONTAINER_BASE CONTAINER_GW <<< "$(python3 - <<PYEOF
import ipaddress
net = ipaddress.IPv6Network('$IPV6_ADDR/$IPV6_PREFIX', strict=False)
base_int = int(net.network_address)
prefix = int('$IPV6_PREFIX')
offset = 0xcafe << (128 - prefix - 16) if prefix <= 112 else 0
container_int = (base_int | offset) & ~((1 << (128 - 112)) - 1)
container_net = ipaddress.IPv6Network(f'{ipaddress.IPv6Address(container_int)}/112', strict=False)
gw = container_net.network_address + 1
print(str(container_net.network_address), str(gw))
PYEOF
)"
    log "$(t "Container subnet: ${CONTAINER_BASE}/112  gateway: ${CONTAINER_GW}" "容器子网: ${CONTAINER_BASE}/112  网关: ${CONTAINER_GW}")"

    if [ -n "$IPV6_IFACE" ]; then
        configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"
    fi

    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
        --ipv6 \
        --subnet="${CONTAINER_BASE}/112" --gateway="$CONTAINER_GW" \
        "$PODMAN_NETWORK"
    # snat_ipv6=false 不能通过 CLI 传入（Podman CLI 有白名单校验），直接注入 JSON
    NETWORK_JSON="/etc/containers/networks/${PODMAN_NETWORK}.json"
    jq '.options["snat_ipv6"] = "false"' "$NETWORK_JSON" > "${NETWORK_JSON}.tmp" \
        && mv "${NETWORK_JSON}.tmp" "$NETWORK_JSON" \
        && log "$(t "✓ snat_ipv6=false injected into network config." "✓ snat_ipv6=false 已注入网络配置。")" \
        || log "$(t "Warning: failed to inject snat_ipv6=false." "警告: 注入 snat_ipv6=false 失败。")"

    NDP_IFACE="$IPV6_IFACE"
    NDP_SUBNETS="${CONTAINER_BASE}/112"
    NDP_NETWORK="$PODMAN_NETWORK"
    log "$(t "✓ NDP responder will be handled by NarwhalCloud Agent." "✓ NDP responder 将由 NarwhalCloud Agent 内置处理。")"
fi

# Podman auto-restart service
cat > /usr/lib/systemd/system/podman-restart.service <<'EOF'
[Unit]
Description=Podman Start All Containers With Restart Policy
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=true
ExecStart=/usr/bin/podman start --all --filter restart-policy=always --filter restart-policy=unless-stopped
ExecStop=/usr/bin/podman stop --all --filter restart-policy=always --filter restart-policy=unless-stopped

[Install]
WantedBy=default.target
EOF
start_service podman-restart

# Pull container images
    log "$(t "Pulling container images..." "拉取容器镜像...")"
    for image in "docker.io/narwhalcloud/debian:podman" "docker.io/narwhalcloud/alpine:podman"; do
        podman images --format "{{.Repository}}:{{.Tag}}" | grep -q "$image" \
            && log "$(t "Image $image already exists." "镜像 $image 已存在。")" \
            || { log "$(t "Pulling $image..." "拉取 $image...")"; podman pull "$image" \
                || log "$(t "Warning: failed to pull $image" "警告: $image 拉取失败")"; }
    done
fi  # End of Podman vs cloud-hypervisor network setup

# ── Cloud-hypervisor specific setup ────────────────────────────────────────────

if [ "$VIRT_TYPE" = "cloudhv" ]; then
    log "$(t "Setting up cloud-hypervisor..." "配置 cloud-hypervisor...")"

    # Ensure agent bin directory exists
    mkdir -p "$AGENT_BIN_DIR"

    # Download cloud-hypervisor binary
    if ! command -v cloud-hypervisor &>/dev/null && ! [ -f "$AGENT_BIN_DIR/cloud-hypervisor" ]; then
        # 调试模式：检查本地文件
        ch_local_name=""
        case "$ARCH" in
            amd64) ch_local_name="cloud-hypervisor-static" ;;
            arm64) ch_local_name="cloud-hypervisor-static-aarch64" ;;
            *) ch_local_name="cloud-hypervisor-static-$ARCH" ;;
        esac

        if [ -f "./$ch_local_name" ]; then
            log "$(t "Found local cloud-hypervisor binary: ./$ch_local_name (debug mode)" "找到本地 cloud-hypervisor 二进制: ./$ch_local_name (调试模式)")"
            cp "./$ch_local_name" "$AGENT_BIN_DIR/cloud-hypervisor"
            chmod +x "$AGENT_BIN_DIR/cloud-hypervisor"
            log "$(t "✓ cloud-hypervisor binary installed (from local)." "✓ cloud-hypervisor 二进制已安装 (来自本地)。")"
        else
            # 生产模式：从官方仓库下载
            log "$(t "Downloading cloud-hypervisor binary from official repository..." "从官方仓库下载 cloud-hypervisor 二进制文件...")"
            download_with_retry "$CLOUD_HYPERVISOR_BASE/$ch_local_name" "$AGENT_BIN_DIR/cloud-hypervisor"
            chmod +x "$AGENT_BIN_DIR/cloud-hypervisor"
            log "$(t "✓ cloud-hypervisor binary installed." "✓ cloud-hypervisor 二进制已安装。")"
        fi
    else
        log "$(t "cloud-hypervisor binary already exists." "cloud-hypervisor 二进制已存在。")"
    fi

    # Download prebuilt base VM images (Debian, Alpine); fallback to local build
    if ! fetch_vm_images; then
        log "$(t "Falling back to local image build for: ${VM_FETCH_FAILED[*]}" "回退到本地构建镜像: ${VM_FETCH_FAILED[*]}")"
        update_vm_images "${VM_FETCH_FAILED[@]}"
    fi
fi

# ── Install agent ─────────────────────────────────────────────────────────────

download_agent "$ARCH"

# 初始化 NDP 参数（Podman public IPv6 模式或 cloud-hypervisor subnet 模式下有值）
NDP_IFACE="${NDP_IFACE:-}"
NDP_SUBNETS="${NDP_SUBNETS:-}"
NDP_NETWORK="${NDP_NETWORK:-}"
if [ "$IPV6_MODE" = "subnet" ]; then
    if [ "$VIRT_TYPE" = "cloudhv" ] || [ "$VIRT_TYPE" = "incus" ]; then
        NDP_IFACE="$IPV6_IFACE"
        NDP_SUBNETS="$IPV6_SUBNET"
    fi
fi

# 生成默认 Web 面板用户名和随机密码
DEFAULT_USER="admin"
DEFAULT_PASS=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 12 2>/dev/null || openssl rand -base64 9 | tr -dc A-Za-z0-9 | head -c 12)

# 生成配置文件（包含所有启动参数、IPv6配置和磁盘限制）
write_config_file "$VIRT_TYPE" "$IPV6_MODE" "$IPV6_SUBNET" "$IPV6_ADDR" "$IPV6_IFACE" "$NDP_IFACE" "$NDP_SUBNETS" "$NDP_NETWORK" "$DEFAULT_USER" "$DEFAULT_PASS"

write_service_file "$VIRT_TYPE"
start_service "$AGENT_SERVICE"

# ── Optional rfw ─────────────────────────────────────────────────────────────

if [ "$INSTALL_RFW_FORCE" = "1" ]; then
    install_rfw 1
else
    install_rfw 0
fi

# ── Done ──────────────────────────────────────────────────────────────────────

if [ "$VIRT_TYPE" = "incus" ]; then
    # 1. 初始化默认存储池
    if ! incus storage show default >/dev/null 2>&1; then
        log "$(t "Initializing Incus default storage pool..." "初始化 Incus 默认存储池...")"
        incus storage create default dir
    fi

    # 2. 初始化默认网桥 incusbr0 (遵循 10.91.0.1/20 标准)
    if ! incus network show incusbr0 >/dev/null 2>&1; then
        log "$(t "Initializing Incus network bridge incusbr0..." "初始化 Incus 网桥 incusbr0...")"
        
        INCUS_IPV4="10.91.0.1/20"
        INCUS_IPV6="none"

        if [ "$IPV6_MODE" = "snat" ]; then
            INCUS_IPV6="fd91:cafe:cafe:10::1/64"
        elif [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ]; then
            # 提取前缀并设置为 ::1/112 网关
            _prefix=$(echo "$IPV6_SUBNET" | sed 's/::\/.*//')
            INCUS_IPV6="${_prefix}::1/112"
        fi
        # IPV6_MODE="none" 时 INCUS_IPV6 保持 "none"，Incus 不配置 IPv6

        incus network create incusbr0 \
            ipv4.address="$INCUS_IPV4" ipv4.nat=true \
            ipv6.address="$INCUS_IPV6" ipv6.nat=$( [ "$IPV6_MODE" = "snat" ] && echo "true" || echo "false" ) \
            ipv6.routing=true \
            ipv6.dhcp=false \
            dns.nameservers="1.1.1.1,2606:4700:4700::1111"
    fi

    # 3. 确保默认配置集 (profile) 使用基础磁盘
    if incus profile show default >/dev/null 2>&1; then
        incus profile device add default root disk path=/ pool=default >/dev/null 2>&1 || true
        # 移除 profile 中的 eth0，由 Agent 在创建实例时动态注入，避免 IP 冲突
        incus profile device remove default eth0 >/dev/null 2>&1 || true
    fi

    # 3.5 本地镜像服务 / 定制 alpine 基础镜像（离线 / 内网部署）
    if [ -n "$INCUS_LOCAL_IMAGE_DIR" ]; then
        import_local_incus_images || log "$(t "Warning: local image import had issues" "警告: 本地镜像导入存在问题")"
    fi
    if [ -n "$INCUS_ALPINE_BASE" ]; then
        import_custom_alpine_base
    fi

    # 3.6 配置 SSH 登录横幅（欢迎页）
    prompt_banner

    # 3.7 WireGuard IPv6 池：subnet 模式下复用同一前缀供 WG 隧道分配
    if [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ] && [ -z "$INCUS_WG_IPV6_SUBNET" ]; then
        INCUS_WG_IPV6_SUBNET="$IPV6_SUBNET"
    fi

    # 4. 导入预构建的 ready 镜像（离线目录优先；否则从私有镜像服务器 / 默认源）
    if [ -n "$INCUS_LOCAL_IMAGE_DIR" ]; then
        log "$(t "Using local image dir, skipping remote import." "使用本地镜像目录，跳过远程导入。")"
    else
        _nc_mirror_ok=0
        if [ -n "$INCUS_IMAGE_MIRROR" ]; then
            if import_incus_images_from_mirror; then
                _nc_mirror_ok=1
            else
                log "$(t "Mirror import skipped/failed; will use configured base / default source." "镜像导入跳过/失败；将使用配置源 / 默认源。")"
            fi
        fi
        if [ "$_nc_mirror_ok" = "1" ]; then
            # 镜像服务器已提供部分发行版：其余（如 debian）从 GitHub 默认源补齐；
            # 若镜像不可达导致前述导入均失败，则此处一并补齐 alpine
            _nc_saved_mirror="$INCUS_IMAGE_MIRROR"
            INCUS_IMAGE_MIRROR="" import_incus_images
            INCUS_IMAGE_MIRROR="$_nc_saved_mirror"
        else
            # 镜像不可用或为非 simplestreams（flat）镜像：按原逻辑使用 --image-mirror 基址 / GitHub
            import_incus_images
        fi
    fi

    # 4.1 注册私有 simplestreams 镜像服务器为 incus remote（best-effort）。
    #     既能让运维直接 `incus launch podcctv-mirror:alpine/3.23`，也顺带
    #     在线验证 index.json 是否已正确（修正前 incus remote add 会失败）。
    if [ -n "$INCUS_IMAGE_MIRROR" ] && command -v incus >/dev/null 2>&1; then
        if incus remote list 2>/dev/null | grep -qw "podcctv-mirror"; then
            log "$(t "Incus remote podcctv-mirror already registered." "incus remote podcctv-mirror 已注册。")"
        elif incus remote add podcctv-mirror "$INCUS_IMAGE_MIRROR" --protocol=simplestreams --public >/dev/null 2>&1; then
            log "$(t "✓ Registered incus remote podcctv-mirror ($INCUS_IMAGE_MIRROR)" "✓ 已注册 incus remote podcctv-mirror ($INCUS_IMAGE_MIRROR)")"
        else
            log "$(t "Warning: could not register podcctv-mirror (index.json missing/unreachable). Runtime build falls back to upstream." "警告: 无法注册 podcctv-mirror（index.json 缺失/不可达）。运行时构建将回退到上游源。")"
        fi
    fi

    # 5. IPv6 回滚辅助脚本（幂等）；配置备份已在“变更前”阶段完成，此处不再覆盖 latest 快照
    if [ "$IPV6_MODE" != "none" ]; then
        install_ipv6_rollback_helper
    fi
fi

IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
echo ""
log "========================================"
log "$(t "✓ NarwhalCloud Agent installation complete!" "✓ NarwhalCloud Agent 安装完成！")"
log "$(t "IP:           $IP" "IP:           $IP")"
log "$(t "Web panel:    http://$IP:$AGENT_WEB_PORT" "面板地址:     http://$IP:$AGENT_WEB_PORT")"
log "$(t "Username:     $DEFAULT_USER" "用户名:       $DEFAULT_USER")"
log "$(t "Password:     $DEFAULT_PASS" "密码:         $DEFAULT_PASS")"
echo ""
log "$(t "Next step: log in to the web panel and enter your Token" "下一步：登录面板并在设置中填入您的 Token")"
log "========================================"
