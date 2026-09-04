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

AGENT_RELEASE_TAG="${AGENT_RELEASE_TAG:-continuous}"
DOWNLOAD_BASE="${RUNMAN_AGENT_DOWNLOAD_BASE:-https://github.com/podcctv/runman-agent/releases/download/$AGENT_RELEASE_TAG}"
CLOUD_HYPERVISOR_BASE="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/latest/download"
# 预构建系统镜像（cloudhv/incus），由 narwhal-cloud/images 仓库 CI 每月构建
VM_IMAGES_BASE="https://github.com/narwhal-cloud/images/releases/download/vm-latest"
NETAVARK_BASE="https://github.com/narwhal-cloud/netavark/releases/latest/download"
# rfw v2 随 agent 同仓库发布，资产名为 rfw-amd64 / rfw-arm64
RFW_BASE="$DOWNLOAD_BASE"
DEFAULT_INCUS_IMAGE_MIRROR="https://alpine-incus-base.428048.xyz"
DEFAULT_INCUS_ALPINE_VERSION="3.24"

# t EN ZH — returns the string for current language
t() { [ "$LANG_CODE" = "zh" ] && echo "$2" || echo "$1"; }

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"; }
die() { log "$1" >&2; exit 1; }

# ── IPv6 配置备份 / 一键回滚 ───────────────────────────────────────────────────
# 备份对象：内核参数、网卡、journald/zram、Incus 网络及本安装器的 systemd 文件。
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
    [ -f /etc/systemd/journald.conf ] && cp /etc/systemd/journald.conf "$dir/journald.conf"
    [ -f /etc/systemd/zram-generator.conf ] && cp /etc/systemd/zram-generator.conf "$dir/zram-generator.conf"
    [ -f /etc/udev/rules.d/99-loop-directio.rules ] && cp /etc/udev/rules.d/99-loop-directio.rules "$dir/loop-directio.rules"
    [ -f /etc/containers/storage.conf ] && cp /etc/containers/storage.conf "$dir/containers-storage.conf"
    [ -f /etc/systemd/system/rfw.service ] && cp /etc/systemd/system/rfw.service "$dir/rfw.service"
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
        echo "HAD_JOURNALD=$( [ -f /etc/systemd/journald.conf ] && echo 1 || echo 0 )"
        echo "HAD_ZRAM=$( [ -f /etc/systemd/zram-generator.conf ] && echo 1 || echo 0 )"
        echo "HAD_LOOP_RULE=$( [ -f /etc/udev/rules.d/99-loop-directio.rules ] && echo 1 || echo 0 )"
        echo "HAD_STORAGE_CONF=$( [ -f /etc/containers/storage.conf ] && echo 1 || echo 0 )"
        echo "HAD_RFW_SERVICE=$( [ -f /etc/systemd/system/rfw.service ] && echo 1 || echo 0 )"
    } > "$dir/meta.env"
    echo "$dir" > "$INCUS_IPV6_BACKUP_DIR/latest"
    echo "$dir"
}

restore_ipv6_config() {
    local target="${1:-$(cat "$INCUS_IPV6_BACKUP_DIR/latest" 2>/dev/null)}"
    [ -n "$target" ] && [ -d "$target" ] || { log "$(t "No IPv6 backup found" "未找到 IPv6 备份")"; return 1; }
    # 元数据只读取固定的 0/1 标记，不 source 文件，避免环境值被当成 shell 执行。
    meta_flag() {
        local name="$1" value
        value=$(sed -n "s/^${name}=\([01]\)$/\1/p" "$target/meta.env" 2>/dev/null | head -1)
        echo "${value:-0}"
    }
    HAD_SYSCTL=$(meta_flag HAD_SYSCTL)
    HAD_MODULES=$(meta_flag HAD_MODULES)
    HAD_INCUSBR0=$(meta_flag HAD_INCUSBR0)
    HAD_JOURNALD=$(meta_flag HAD_JOURNALD)
    HAD_ZRAM=$(meta_flag HAD_ZRAM)
    HAD_LOOP_RULE=$(meta_flag HAD_LOOP_RULE)
    HAD_STORAGE_CONF=$(meta_flag HAD_STORAGE_CONF)
    HAD_RFW_SERVICE=$(meta_flag HAD_RFW_SERVICE)
    # sysctl：有原文件则恢复；无原文件（安装时新建）则直接删除
    if [ -f "$target/sysctl.conf" ]; then
        cp "$target/sysctl.conf" /etc/sysctl.d/99-narwhalcloud.conf && sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1
    elif [ "${HAD_SYSCTL:-0}" = "0" ] && [ -f /etc/sysctl.d/99-narwhalcloud.conf ]; then
        rm -f /etc/sysctl.d/99-narwhalcloud.conf && sysctl --system >/dev/null 2>&1
        log "$(t "Removed install-created sysctl file" "已删除安装时新建的 sysctl 配置文件")"
    fi
    # interfaces：仅当备份存在时恢复（避免误删用户原有配置）
    [ -f "$target/interfaces" ] && cp "$target/interfaces" /etc/network/interfaces
    # 安全卸载会保留后端容器，因此也必须保留其依赖的 Incus 网桥。
    # 完整清理/显式回滚时才恢复安装前的 incusbr0 状态。
    if [ "${PRESERVE_INCUS_NETWORK:-0}" = "1" ]; then
        log "$(t "Preserved incusbr0 because backend instances are being kept." "已保留 incusbr0，避免影响保留的后端实例。")"
    elif command -v incus >/dev/null 2>&1; then
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

    # 其它受管文件：安装前存在则恢复原文件；安装时新建则删除。
    restore_managed_file() {
        local backup="$1" destination="$2" existed="$3"
        if [ -f "$target/$backup" ]; then
            mkdir -p "$(dirname "$destination")"
            cp "$target/$backup" "$destination"
        elif [ "$existed" = "0" ]; then
            rm -f "$destination"
        fi
    }
    restore_managed_file journald.conf /etc/systemd/journald.conf "$HAD_JOURNALD"
    restore_managed_file zram-generator.conf /etc/systemd/zram-generator.conf "$HAD_ZRAM"
    restore_managed_file loop-directio.rules /etc/udev/rules.d/99-loop-directio.rules "$HAD_LOOP_RULE"
    if [ "${PRESERVE_PODMAN_STORAGE:-0}" = "1" ]; then
        log "$(t "Preserved Podman storage.conf so retained containers remain manageable." "已保留 Podman storage.conf，确保保留的容器仍可管理。")"
    else
        restore_managed_file containers-storage.conf /etc/containers/storage.conf "$HAD_STORAGE_CONF"
    fi
    restore_managed_file rfw.service /etc/systemd/system/rfw.service "$HAD_RFW_SERVICE"
    systemctl daemon-reload 2>/dev/null || true
    systemctl restart systemd-journald 2>/dev/null || true
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

# ── Token 管理 ──────────────────────────────────────────────────────────────

generate_agent_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 24
    else
        tr -dc 'A-Za-z0-9' </dev/urandom | head -c 48
        echo
    fi
}

validate_agent_token() {
    local value="$1" length=${#1}
    [ "$length" -ge 16 ] && [ "$length" -le 512 ] || {
        log "$(t "Token must contain 16-512 characters." "Token 长度必须为 16-512 个字符。")" >&2
        return 1
    }
    case "$value" in
        *$'\n'*|*$'\r'*|*$'\t'*)
            log "$(t "Token must not contain control characters." "Token 不能包含换行或制表符。")" >&2
            return 1
            ;;
    esac
}

read_agent_token() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 1
    jq -r '.token // empty' "$AGENT_CONFIG_FILE" 2>/dev/null
}

write_agent_token() {
    local value="$1"
    [ "$(id -u)" = "0" ] || die "$(t "Token management requires root." "Token 管理需要 root 权限。")"
    [ -f "$AGENT_CONFIG_FILE" ] || die "$(t "Agent config not found: $AGENT_CONFIG_FILE" "未找到 Agent 配置: $AGENT_CONFIG_FILE")"
    command -v jq >/dev/null 2>&1 || die "jq is required"
    validate_agent_token "$value" || exit 1
    jq --arg token "$value" '.token = $token' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp"
    chmod 600 "$AGENT_CONFIG_FILE.tmp"
    mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE"
    systemctl restart "$AGENT_SERVICE" 2>/dev/null || true
}

show_agent_token() {
    [ "$(id -u)" = "0" ] || die "$(t "Showing the Token requires root." "查看 Token 需要 root 权限。")"
    local value
    value=$(read_agent_token) || die "$(t "Agent config not found." "未找到 Agent 配置。")"
    [ -n "$value" ] || die "$(t "The integration Token is empty." "对接 Token 尚未设置。")"
    printf 'NARWHAL_AGENT_TOKEN=%s\n' "$value"
}

rotate_agent_token() {
    local value="${TOKEN_VALUE:-${NARWHAL_AGENT_TOKEN:-}}"
    if [ -z "$value" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        read -rsp "$(t "New Token (leave blank to generate): " "新 Token（留空自动生成）: ")" value
        echo
    fi
    [ -n "$value" ] || value=$(generate_agent_token)
    write_agent_token "$value"
    log "$(t "✓ Integration Token rotated; agent restarted." "✓ 对接 Token 已轮换，Agent 已重启。")"
    printf 'NARWHAL_AGENT_TOKEN=%s\n' "$value"
    log "$(t "Rotate again: bash install.sh --rotate-token" "再次轮换: bash install.sh --rotate-token")"
}

# ── Incus 制品清理 ──────────────────────────────────────────────────────────

purge_incus_artifacts() {
    command -v incus >/dev/null 2>&1 || {
        log "$(t "Incus is not installed; nothing to purge." "未安装 Incus，无需清理。")"
        return 0
    }

    local names name alias answer purge_arch failed=0
    case "$(uname -m)" in
        aarch64|arm64) purge_arch="arm64" ;;
        *) purge_arch="amd64" ;;
    esac
    names=$(incus list --format csv -c n 2>/dev/null || true)
    log "$(t "Incus instances to delete:" "将删除的 Incus 实例:") ${names:-<none>}"
    log "$(t "Managed image aliases to delete:" "将删除的受管镜像别名:") alpine/3.24/cloud/$purge_arch/ready, alpine/3.23/cloud/$purge_arch/ready, debian/13/cloud/$purge_arch/ready"
    log "$(t "Managed network/remote to delete:" "将删除的受管网络/远端:") incusbr0, podcctv-mirror"

    if [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        read -rp "$(t "Type PURGE to continue: " "输入 PURGE 继续: ")" answer
        [ "$answer" = "PURGE" ] || die "$(t "Purge cancelled." "已取消彻底清理。")"
    fi

    while IFS= read -r name; do
        [ -n "$name" ] || continue
        if ! incus delete "$name" --force >/dev/null 2>&1; then
            log "$(t "Failed to delete Incus instance:" "删除 Incus 实例失败:") $name"
            failed=1
        fi
    done <<< "$names"

    for alias in "alpine/3.24/cloud/$purge_arch/ready" "alpine/3.23/cloud/$purge_arch/ready" "debian/13/cloud/$purge_arch/ready" podcctv/alpine-base; do
        incus image info "$alias" >/dev/null 2>&1 || continue
        if ! incus image delete "$alias" >/dev/null 2>&1; then
            log "$(t "Failed to delete managed Incus image:" "删除受管 Incus 镜像失败:") $alias"
            failed=1
        fi
    done
    if incus remote list --format csv -c n 2>/dev/null | grep -qx podcctv-mirror; then
        incus remote remove podcctv-mirror >/dev/null 2>&1 || {
            log "$(t "Failed to remove Incus remote: podcctv-mirror" "删除 Incus 远端失败: podcctv-mirror")"
            failed=1
        }
    fi
    if incus network show incusbr0 >/dev/null 2>&1; then
        incus network delete incusbr0 >/dev/null 2>&1 || {
            log "$(t "Failed to delete incusbr0; check for remaining instances or profiles." "删除 incusbr0 失败；请检查残留实例或 profile 引用。")"
            failed=1
        }
    fi
    [ "$failed" = "0" ] || die "$(t "Incus purge was incomplete; see the failed items above." "Incus 清理未完成，请检查上方失败项目。")"
    log "$(t "✓ Managed Incus instances, images, network and remote removed." "✓ 已删除受管 Incus 实例、镜像、网络和远端。")"
}

# ── 一键卸载 ────────────────────────────────────────────────────────────────
# 仅撤销本安装器引入的变更，不影响既有业务（其它服务、incus 容器/镜像默认保留）。
# 最关键的是先恢复 IPv6 / 网卡配置（基于安装前的完整备份），再停止服务、清理文件。
do_uninstall() {
    [ "$(id -u)" = "0" ] || die "$(t "Uninstall requires root." "卸载需要 root 权限。")"
    log "$(t "=== NarwhalCloud Agent uninstall ===" "=== NarwhalCloud Agent 卸载 ===")"
    local install_origin_restored=0 previous_virt="unknown"
    local PRESERVE_INCUS_NETWORK=0 PRESERVE_PODMAN_STORAGE=0
    if [ -f "$AGENT_CONFIG_FILE" ]; then
        previous_virt=$(grep -o '"virt_type"[[:space:]]*:[[:space:]]*"[^"]*"' "$AGENT_CONFIG_FILE" 2>/dev/null \
            | head -n1 | sed 's/.*:[[:space:]]*"//; s/"$//' || true)
    fi

    # 普通卸载承诺保留后端状态。不要恢复掉保留实例正在使用的网桥，
    # 也不要删除指向 /data 的 Podman storage.conf。
    if [ "${PURGE_INCUS:-0}" != "1" ]; then
        [ "$previous_virt" = "incus" ] && PRESERVE_INCUS_NETWORK=1
        [ "$previous_virt" = "podman" ] && PRESERVE_PODMAN_STORAGE=1
    fi

    # 完整重装模式先删实例，避免恢复/删除网桥时仍被实例引用。
    [ "${PURGE_INCUS:-0}" = "1" ] && purge_incus_artifacts

    # 0. 最先恢复 IPv6 / 网卡配置（最关键：避免遗留错误路由/转发影响业务）
    if [ -d "$INCUS_IPV6_BACKUP_DIR" ]; then
        local origin
        origin=$(cat "$INCUS_IPV6_BACKUP_DIR/install-origin" 2>/dev/null || true)
        if [ -n "$origin" ] && restore_ipv6_config "$origin"; then
            install_origin_restored=1
        else
            log "$(t "System config restore skipped (no install-origin backup)." "系统配置恢复跳过（无安装前基线备份）。")"
        fi
    fi

    # 1. 停止并禁用服务（仅本 agent，不动其它业务服务）
    if command -v systemctl >/dev/null 2>&1; then
        systemctl stop "$AGENT_SERVICE" 2>/dev/null || true
        systemctl disable "$AGENT_SERVICE" 2>/dev/null || true
        # rfw 是本安装器的可选组件；安装前若已存在，其 service 文件已在上一步恢复。
        if [ "$install_origin_restored" = "1" ] && [ "${HAD_RFW_SERVICE:-0}" = "0" ]; then
            systemctl stop rfw 2>/dev/null || true
            systemctl disable rfw 2>/dev/null || true
            rm -f /etc/systemd/system/rfw.service
        fi
        if [ -x /usr/local/sbin/runman-podman-forwarding ]; then
            /usr/local/sbin/runman-podman-forwarding remove 2>/dev/null || true
        fi
        systemctl stop runman-podman-forwarding 2>/dev/null || true
        systemctl disable runman-podman-forwarding 2>/dev/null || true
    fi
    rm -f "/etc/systemd/system/${AGENT_SERVICE}.service"
    rm -f /etc/systemd/system/runman-podman-forwarding.service
    rm -f /usr/local/sbin/runman-podman-forwarding
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

    if [ "${PURGE_INCUS:-0}" = "1" ]; then
        log "$(t "Full uninstall complete; managed Incus artifacts were removed." "完整卸载完成；受管 Incus 制品已删除。")"
    elif [ "$previous_virt" = "podman" ]; then
        log "$(t "Uninstall complete. Podman containers, narwhal-net, images and /data are preserved." \
            "卸载完成。已保留 Podman 容器、narwhal-net、镜像与 /data 数据盘。")"
    else
        log "$(t "Uninstall complete. Incus containers/images and other services are preserved." "卸载完成。已保留 incus 容器/镜像及其它业务服务。")"
    fi
    log "$(t "Backups kept at: $INCUS_IPV6_BACKUP_DIR (rerun install.sh --rollback-ipv6 if needed)" "备份保留于: $INCUS_IPV6_BACKUP_DIR（如有需要可重新运行 install.sh --rollback-ipv6）")"
    if [ "${PURGE_INCUS:-0}" != "1" ] && [ "$previous_virt" = "incus" ]; then
        log "$(t "For a clean Incus reinstall, use: bash install.sh --uninstall --purge-incus" "如需彻底清理 Incus 后重装: bash install.sh --uninstall --purge-incus")"
    fi
}

# ── Language selection ────────────────────────────────────────────────────────

# Support non-interactive mode via environment variable or command line argument
INSTALL_RFW_FORCE=0
# 1: 忽略版本戳，强制重新下载 cloudhv / incus 预构建镜像
FORCE_IMAGE_REFRESH="${FORCE_IMAGE_REFRESH:-0}"

# 本地 incus 镜像服务 / 定制能力相关环境变量（离线/内网部署用）
[ "${INCUS_IMAGE_MIRROR+x}" = x ] && INCUS_IMAGE_MIRROR_EXPLICIT=1 || INCUS_IMAGE_MIRROR_EXPLICIT=0
INCUS_IMAGE_MIRROR="${INCUS_IMAGE_MIRROR:-$DEFAULT_INCUS_IMAGE_MIRROR}"  # 私有 simplestreams 镜像服务器（fork 默认）；留空则使用 GitHub releases
INCUS_LOCAL_IMAGE_DIR="${INCUS_LOCAL_IMAGE_DIR:-}" # 本地镜像目录（含 incus-<distro>-<arch>.tar.gz），直接离线导入
INCUS_ALPINE_BASE="${INCUS_ALPINE_BASE:-}"         # 定制 alpine 基础镜像：本地 tar.gz 路径或已存在的 incus 别名
INCUS_IPV6_ALLOC="${INCUS_IPV6_ALLOC:-1}"          # 每个容器分配的 IPv6 数量（非 /64 网段精细化分配）
INCUS_WG_IPV6_SUBNET="${INCUS_WG_IPV6_SUBNET:-}"   # 供 WireGuard 隧道分配的 IPv6 池（CIDR）
INCUS_BANNER_PRESET="${INCUS_BANNER_PRESET:-none}" # none / default / minimal / project / custom
INCUS_BANNER_TEXT="${INCUS_BANNER_TEXT:-}"         # preset=custom 时的完整横幅文本
INCUS_IPV6_BACKUP_DIR="${INCUS_IPV6_BACKUP_DIR:-/var/lib/narwhal-agent/backups}" # IPv6 配置备份目录
INCUS_IPV6_ONLY="${INCUS_IPV6_ONLY:-}" # 设为 1 时新建容器为纯 IPv6（不分配 IPv4），需 IPv6 模式为 subnet/snat
IPV6_ROUTED="${IPV6_ROUTED:-0}"         # 1=独立 routed prefix（6in4/WireGuard 等），无需上游 NDP
[ "${PODMAN_REGISTRY_MIRROR+x}" = x ] && PODMAN_REGISTRY_MIRROR_EXPLICIT=1 || PODMAN_REGISTRY_MIRROR_EXPLICIT=0
PODMAN_DATA_SIZE="${PODMAN_DATA_SIZE:-}" # Podman XFS 数据盘大小；留空时按根分区可用空间推荐
PODMAN_REGISTRY_MIRROR="${PODMAN_REGISTRY_MIRROR:-}" # docker.io registry mirror，例如 mirror.example.com
VIRT_TYPE_REQUESTED="${VIRT_TYPE_REQUESTED:-}"
NON_INTERACTIVE="${NON_INTERACTIVE:-0}"
ORIGINAL_ARGC=$#
FORCE_MENU=0
UPDATE_ONLY=0
UPDATE_NETWORK_REQUESTED=0
GUIDED_INSTALL=0
PURGE_INCUS="${PURGE_INCUS:-0}"
TOKEN_ACTION=""
TOKEN_VALUE="${TOKEN_VALUE:-}"
INITIAL_TOKEN="${NARWHAL_AGENT_TOKEN:-}"
GENERATE_TOKEN=0
SKIP_IPV6_PROBE="${SKIP_IPV6_PROBE:-0}"
SYSTEM_ACTION=""
IMAGE_MENU=0

# 一键模式：IPv6 探测 / 备份 / 回滚，处理后退出
IPV6_ONESHOT_MODE="${IPV6_ONESHOT_MODE:-}"
# 一键卸载模式：--uninstall 撤销本安装器引入的全部变更（含 IPv6 / 网卡恢复）后退出
UNINSTALL="${UNINSTALL:-}"

show_help() {
    cat <<'EOF'
Runman Agent installer

  bash install.sh                         Guided menu
  bash install.sh --virt incus            Install/update Incus backend
  bash install.sh --update-only           Back up and update Agent only; require an existing installation
  bash install.sh --detect-ipv6            Detect native/tunnel IPv6 and routed prefix
  bash install.sh --validate-ipv6 ...      Validate manually supplied IPv6 values
  bash install.sh --show-token             Print current integration Token
  bash install.sh --rotate-token           Generate, store and print a new Token
  bash install.sh --rotate-token --token X Store and print a custom Token
  bash install.sh --status                 Show Agent/backend/service status
  bash install.sh --restart-agent          Restart Agent and optional rfw
  bash install.sh --reset-panel-password   Reset panel password interactively
  bash install.sh --uninstall              Remove Agent; preserve backend containers/images/data
  bash install.sh --uninstall --purge-incus
                                           Remove Agent and managed Incus artifacts

IPv6: --ipv6-mode none|snat|subnet --ipv6-addr ADDR
      --ipv6-subnet CIDR --ipv6-iface IFACE --ipv6-routed
      --ipv6-only | --nat4 --skip-ipv6-probe
Podman: --data-size 10G --podman-registry-mirror mirror.example.com
EOF
}

show_agent_status() {
    local virt="not-installed" agent_state="inactive" backend_state="n/a" rfw_state="not-installed" panel_host
    if [ -f "$AGENT_CONFIG_FILE" ] && command -v jq >/dev/null 2>&1; then
        virt=$(jq -r '.virt_type // "unknown"' "$AGENT_CONFIG_FILE" 2>/dev/null || echo unknown)
    fi
    command -v systemctl >/dev/null 2>&1 && agent_state=$(systemctl is-active "$AGENT_SERVICE" 2>/dev/null || true)
    case "$virt" in
        podman) backend_state=$(systemctl is-active podman.socket 2>/dev/null || true) ;;
        incus) backend_state=$(systemctl is-active incus 2>/dev/null || true) ;;
        cloudhv) backend_state=$([ -e /dev/kvm ] && echo ready || echo no-kvm) ;;
    esac
    [ -f "$RFW_BIN_DIR/rfw" ] && rfw_state=$(systemctl is-active rfw 2>/dev/null || true)
    panel_host=$(curl -4 -s --max-time 5 ip.sb 2>/dev/null | head -n1 || true)
    [ -n "$panel_host" ] || panel_host='<host-ip>'
    case "$panel_host" in *:*) panel_host="[$panel_host]" ;; esac
    printf '%s\n' \
        "VIRT_TYPE=$virt" \
        "AGENT_SERVICE=$agent_state" \
        "BACKEND_SERVICE=$backend_state" \
        "RFW_SERVICE=$rfw_state" \
        "PANEL=http://$panel_host:$AGENT_WEB_PORT"
}

ensure_podman_network_from_config() {
    [ -f "$AGENT_CONFIG_FILE" ] || return 0
    command -v jq >/dev/null 2>&1 && command -v podman >/dev/null 2>&1 || return 0
    [ "$(jq -r '.virt_type // ""' "$AGENT_CONFIG_FILE" 2>/dev/null)" = "podman" ] || return 0
    podman network exists "$PODMAN_NETWORK" 2>/dev/null && return 0

    local mode subnet container_base container_gw network_json
    mode=$(jq -r '.ipv6_mode // "none"' "$AGENT_CONFIG_FILE")
    log "$(t "Podman network $PODMAN_NETWORK is missing; rebuilding it from config." \
        "Podman 网络 $PODMAN_NETWORK 缺失，正在按现有配置重建。")"
    case "$mode" in
        none)
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 "$PODMAN_NETWORK"
            ;;
        snat)
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
                --ipv6 --subnet=fd91:cafe:cafe:10::/64 --gateway=fd91:cafe:cafe:10::1 \
                "$PODMAN_NETWORK"
            ;;
        subnet)
            subnet=$(jq -r '.ipv6_subnet // ""' "$AGENT_CONFIG_FILE")
            [ -n "$subnet" ] || die "$(t "Cannot rebuild Podman subnet network: ipv6_subnet is empty." \
                "无法重建 Podman 子网：ipv6_subnet 为空。")"
            read -r container_base container_gw <<< "$(python3 - "$subnet" <<'PYEOF'
import ipaddress
import sys

net = ipaddress.IPv6Network(sys.argv[1], strict=False)
offset = 0xcafe << (128 - net.prefixlen - 16) if net.prefixlen <= 112 else 0
container_int = (int(net.network_address) | offset) & ~((1 << 16) - 1)
container_net = ipaddress.IPv6Network(f'{ipaddress.IPv6Address(container_int)}/112', strict=False)
print(container_net.network_address, container_net.network_address + 1)
PYEOF
)"
            podman network create --driver=bridge \
                --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
                --ipv6 --subnet="${container_base}/112" --gateway="$container_gw" \
                "$PODMAN_NETWORK"
            network_json="/etc/containers/networks/${PODMAN_NETWORK}.json"
            jq '.options["snat_ipv6"] = "false"' "$network_json" > "${network_json}.tmp" \
                && mv "${network_json}.tmp" "$network_json"
            ;;
        *) die "$(t "Unsupported Podman IPv6 mode in config: $mode" "配置中的 Podman IPv6 模式无效: $mode")" ;;
    esac
    log "$(t "Podman network $PODMAN_NETWORK rebuilt." "Podman 网络 $PODMAN_NETWORK 已重建。")"
}

restart_agent_services() {
    [ -f "$AGENT_BINARY" ] || die "$(t "Agent is not installed." "Agent 尚未安装。")"
    ensure_podman_network_from_config
    systemctl restart "$AGENT_SERVICE"
    [ -f "$RFW_BIN_DIR/rfw" ] && systemctl restart rfw 2>/dev/null || true
    log "$(t "Agent services restarted." "Agent 服务已重启。")"
    show_agent_status
}

reset_panel_password() {
    [ -x "$AGENT_BINARY" ] && [ -f "$AGENT_CONFIG_FILE" ] \
        || die "$(t "Agent is not installed." "Agent 尚未安装。")"
    [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ] \
        || die "$(t "Panel password reset requires an interactive terminal." "重置面板密码需要交互式终端。")"
    local first second
    read -rsp "$(t "New panel password (minimum 8 characters): " "新面板密码（至少 8 个字符）: ")" first; echo
    read -rsp "$(t "Confirm new panel password: " "再次输入新面板密码: ")" second; echo
    [ "$first" = "$second" ] || die "$(t "Passwords do not match." "两次密码不一致。")"
    [ "${#first}" -ge 8 ] || die "$(t "Password must contain at least 8 characters." "密码至少需要 8 个字符。")"
    "$AGENT_BINARY" --config "$AGENT_CONFIG_FILE" --reset-password "$first" >/dev/null
    systemctl restart "$AGENT_SERVICE"
    log "$(t "Panel password reset; Agent restarted." "面板密码已重置，Agent 已重启。")"
}

show_main_menu() {
    printf '\n%s\n' "$(t "Runman Agent guided operations" "Runman Agent 菜单引导")"
    printf '  1) %s\n' "$(t "Install/update (guided network setup)" "安装/更新（网络引导配置）")"
    printf '  2) %s\n' "$(t "Show installation/service status" "查看安装/服务状态")"
    printf '  3) %s\n' "$(t "Repair network and restart Agent/firewall" "修复网络并重启 Agent/防火墙")"
    printf '  4) %s\n' "$(t "Reset Web panel password" "重置 Web 面板密码")"
    printf '  5) %s\n' "$(t "Detect IPv6 only" "仅探测 IPv6")"
    printf '  6) %s\n' "$(t "Validate a manual IPv6 /64" "验证手工 IPv6 /64")"
    printf '  7) %s\n' "$(t "Show integration Token" "显示对接 Token")"
    printf '  8) %s\n' "$(t "Rotate integration Token" "轮换对接 Token")"
    printf '  9) %s\n' "$(t "Back up IPv6 configuration" "备份 IPv6 配置")"
    printf ' 10) %s\n' "$(t "Roll back IPv6 configuration" "回滚 IPv6 配置")"
    printf ' 11) %s\n' "$(t "Uninstall Agent (keep containers/images)" "卸载 Agent（保留容器/镜像）")"
    printf ' 12) %s\n' "$(t "Full Incus cleanup and uninstall" "清理 Incus 制品并完整卸载")"
    printf ' 13) %s\n' "$(t "Configure/refresh container images" "配置/刷新容器镜像")"
    printf '  0) %s\n' "$(t "Exit" "退出")"
    read -rp '> ' _menu_choice
    case "${_menu_choice}" in
        1) GUIDED_INSTALL=1 ;;
        2) SYSTEM_ACTION="status" ;;
        3) SYSTEM_ACTION="restart" ;;
        4) SYSTEM_ACTION="reset-password" ;;
        5) IPV6_ONESHOT_MODE="detect" ;;
        6) IPV6_ONESHOT_MODE="validate" ;;
        7) TOKEN_ACTION="show" ;;
        8) TOKEN_ACTION="rotate" ;;
        9) IPV6_ONESHOT_MODE="backup" ;;
        10) IPV6_ONESHOT_MODE="rollback" ;;
        11) UNINSTALL=1 ;;
        12) UNINSTALL=1; PURGE_INCUS=1 ;;
        13) GUIDED_INSTALL=1; FORCE_IMAGE_REFRESH=1; IMAGE_MENU=1 ;;
        0) exit 0 ;;
        *) die "$(t "Invalid menu selection." "无效的菜单选项。")" ;;
    esac
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        zh) LANG_CODE="zh"; shift ;;
        en) LANG_CODE="en"; shift ;;
        --virt) [ $# -ge 2 ] || die "--virt requires a value"; VIRT_TYPE_REQUESTED="$2"; shift 2 ;;
        --install-rfw) INSTALL_RFW_FORCE=1; shift ;;
        --force-images) FORCE_IMAGE_REFRESH=1; shift ;;
        --image-mirror) [ $# -ge 2 ] || die "--image-mirror requires a value"; INCUS_IMAGE_MIRROR="$2"; INCUS_IMAGE_MIRROR_EXPLICIT=1; shift 2 ;;
        --local-image-dir) [ $# -ge 2 ] || die "--local-image-dir requires a value"; INCUS_LOCAL_IMAGE_DIR="$2"; shift 2 ;;
        --alpine-base) [ $# -ge 2 ] || die "--alpine-base requires a value"; INCUS_ALPINE_BASE="$2"; shift 2 ;;
        --data-size) [ $# -ge 2 ] || die "--data-size requires a value"; PODMAN_DATA_SIZE="$2"; shift 2 ;;
        --podman-registry-mirror) [ $# -ge 2 ] || die "--podman-registry-mirror requires a value"; PODMAN_REGISTRY_MIRROR="$2"; PODMAN_REGISTRY_MIRROR_EXPLICIT=1; shift 2 ;;
        --ipv6-alloc) [ $# -ge 2 ] || die "--ipv6-alloc requires a value"; INCUS_IPV6_ALLOC="$2"; shift 2 ;;
        --wg-ipv6-subnet) [ $# -ge 2 ] || die "--wg-ipv6-subnet requires a value"; INCUS_WG_IPV6_SUBNET="$2"; shift 2 ;;
        --ipv6-mode) [ $# -ge 2 ] || die "--ipv6-mode requires a value"; IPV6_MODE="$2"; shift 2 ;;
        --ipv6-addr) [ $# -ge 2 ] || die "--ipv6-addr requires a value"; IPV6_ADDR="$2"; shift 2 ;;
        --ipv6-subnet) [ $# -ge 2 ] || die "--ipv6-subnet requires a value"; IPV6_SUBNET="$2"; shift 2 ;;
        --ipv6-iface) [ $# -ge 2 ] || die "--ipv6-iface requires a value"; IPV6_IFACE="$2"; shift 2 ;;
        --ipv6-routed) IPV6_ROUTED=1; shift ;;
        --skip-ipv6-probe) SKIP_IPV6_PROBE=1; shift ;;
        --banner-preset) [ $# -ge 2 ] || die "--banner-preset requires a value"; INCUS_BANNER_PRESET="$2"; shift 2 ;;
        --banner-text) [ $# -ge 2 ] || die "--banner-text requires a value"; INCUS_BANNER_TEXT="$2"; shift 2 ;;
        --ipv6-only) INCUS_IPV6_ONLY=1; UPDATE_NETWORK_REQUESTED=1; shift ;;
        --nat4) INCUS_IPV6_ONLY=0; UPDATE_NETWORK_REQUESTED=1; shift ;;
        --token) [ $# -ge 2 ] || die "--token requires a value"; INITIAL_TOKEN="$2"; TOKEN_VALUE="$2"; shift 2 ;;
        --generate-token) GENERATE_TOKEN=1; shift ;;
        -y|--yes|--non-interactive) NON_INTERACTIVE=1; shift ;;
        --menu) FORCE_MENU=1; shift ;;
        --update-only) UPDATE_ONLY=1; shift ;;
        --detect-ipv6) IPV6_ONESHOT_MODE="detect"; shift ;;
        --validate-ipv6) IPV6_ONESHOT_MODE="validate"; shift ;;
        --backup-ipv6) IPV6_ONESHOT_MODE="backup"; shift ;;
        --rollback-ipv6) IPV6_ONESHOT_MODE="rollback"; shift ;;
        --show-token) TOKEN_ACTION="show"; shift ;;
        --rotate-token) TOKEN_ACTION="rotate"; shift ;;
        --status) SYSTEM_ACTION="status"; shift ;;
        --restart-agent) SYSTEM_ACTION="restart"; shift ;;
        --reset-panel-password) SYSTEM_ACTION="reset-password"; shift ;;
        --uninstall) UNINSTALL=1; shift ;;
        --purge-incus) PURGE_INCUS=1; UNINSTALL=1; shift ;;
        -h|--help) show_help; exit 0 ;;
        *) die "Unknown option: $1" ;;
    esac
done

case "$VIRT_TYPE_REQUESTED" in
    ""|podman|cloudhv|incus) ;;
    *) die "Invalid --virt value '$VIRT_TYPE_REQUESTED' (expected podman, cloudhv, or incus)" ;;
esac
case "${IPV6_MODE:-}" in
    ""|none|snat|subnet) ;;
    *) die "Invalid IPv6 mode '${IPV6_MODE}' (expected none, snat, or subnet)" ;;
esac
case "$INCUS_IPV6_ALLOC" in
    ''|*[!0-9]*) die "--ipv6-alloc must be an integer from 1 to 15" ;;
esac
[ "$INCUS_IPV6_ALLOC" -ge 1 ] && [ "$INCUS_IPV6_ALLOC" -le 15 ] \
    || die "--ipv6-alloc must be an integer from 1 to 15"

# One-shot operations and explicit non-interactive installs must never pause for
# a language selection. Guided/menu installs keep the bilingual prompt.
if [ -z "$LANG_CODE" ] && { [ -n "$SYSTEM_ACTION" ] || [ -n "$TOKEN_ACTION" ] \
    || [ -n "$IPV6_ONESHOT_MODE" ] || [ "$UNINSTALL" = "1" ] \
    || [ "$NON_INTERACTIVE" = "1" ]; }; then
    LANG_CODE="en"
fi
if [ -z "$LANG_CODE" ]; then
    printf "Select language / 选择语言:\n  1) English (default)\n  2) 中文\n"
    if [ -t 0 ]; then
        read -t 10 -rp "> " _lang_choice || _lang_choice=1
    else
        _lang_choice=1
    fi
    case "${_lang_choice}" in
        2) LANG_CODE="zh" ;;
        *) LANG_CODE="en" ;;
    esac
fi

# 无参数且有终端时默认打开总菜单；--menu 可显式打开。
if [ "$FORCE_MENU" = "1" ] || { [ "$ORIGINAL_ARGC" -eq 0 ] && [ -t 0 ]; }; then
    show_main_menu
fi

# ── 一键 Token / 备份 / 回滚 / 卸载（处理完即退出）──
if [ "$UPDATE_ONLY" = "1" ]; then
    [ -z "$SYSTEM_ACTION" ] && [ -z "$TOKEN_ACTION" ] && [ -z "$IPV6_ONESHOT_MODE" ] \
        && [ "$UNINSTALL" != "1" ] && [ "$GENERATE_TOKEN" = "0" ] \
        || die "--update-only cannot be combined with other management actions."
fi
case "$SYSTEM_ACTION" in
    status) show_agent_status; exit 0 ;;
    restart) restart_agent_services; exit 0 ;;
    reset-password) reset_panel_password; exit 0 ;;
esac
case "$TOKEN_ACTION" in
    show) show_agent_token; exit 0 ;;
    rotate) rotate_agent_token; exit 0 ;;
esac
if [ "$IPV6_ONESHOT_MODE" = "backup" ]; then
    b=$(backup_ipv6_config); log "$(t "IPv6 config backed up to $b" "IPv6 配置已备份至 $b")"; exit 0
elif [ "$IPV6_ONESHOT_MODE" = "rollback" ]; then
    restore_ipv6_config; exit 0
fi
if [ "$UNINSTALL" = "1" ]; then
    do_uninstall
    exit 0
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

validate_ipv6_values() {
    local mode="$1" addr="$2" subnet="$3" iface="$4"
    case "$mode" in
        none) return 0 ;;
        snat|subnet) ;;
        *) log "$(t "Invalid IPv6 mode: $mode" "无效 IPv6 模式: $mode")" >&2; return 1 ;;
    esac
    [ -n "$addr" ] || { log "$(t "IPv6 address is required." "必须填写 IPv6 地址。")" >&2; return 1; }
    [ -n "$iface" ] || { log "$(t "IPv6 interface is required." "必须填写 IPv6 网卡。")" >&2; return 1; }
    case "$iface" in
        *[!A-Za-z0-9_.:@-]*) log "$(t "Invalid interface name: $iface" "无效网卡名: $iface")" >&2; return 1 ;;
    esac
    ip link show dev "$iface" >/dev/null 2>&1 || {
        log "$(t "Interface does not exist: $iface" "网卡不存在: $iface")" >&2
        return 1
    }
    python3 - "$mode" "$addr" "$subnet" <<'PY'
import ipaddress
import sys

mode, address, subnet = sys.argv[1:]
addr = ipaddress.IPv6Address(address)
if mode == "subnet":
    if not subnet:
        raise SystemExit("IPv6 subnet is required for subnet mode")
    net = ipaddress.IPv6Network(subnet, strict=False)
    if net.prefixlen > 64:
        raise SystemExit(f"subnet mode requires /64 or larger allocation, got /{net.prefixlen}")
    if addr not in net:
        raise SystemExit(f"host address {addr} is not inside {net}")
PY
}

# 临时绑定一个子网地址并以该地址访问公网，完成后立即清理。
probe_ipv6_values() {
    local mode="$1" addr="$2" subnet="$3" iface="$4" routed="$5"
    [ "$SKIP_IPV6_PROBE" = "1" ] && {
        log "$(t "IPv6 online probe skipped by request." "已按要求跳过 IPv6 在线探测。")"
        return 0
    }

    local source_addr bind_cidr bind_iface connected=0 endpoint
    if [ "$mode" = "snat" ]; then
        source_addr="$addr"
        for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
            curl -6 --interface "$source_addr" -fsS --max-time 8 "$endpoint" >/dev/null 2>&1 && connected=1 && break
        done
    else
        source_addr=$(python3 - "$subnet" <<'PY'
import ipaddress
import sys
net = ipaddress.IPv6Network(sys.argv[1], strict=False)
candidate = net.network_address + min(0xfffe, net.num_addresses - 2)
print(candidate)
PY
)
        if ip -6 addr show | grep -Fq " $source_addr/"; then
            source_addr=$(ipv6_plus_one "$source_addr")
        fi
        if [ "$routed" = "1" ]; then
            bind_iface="lo"
            bind_cidr="$source_addr/128"
        else
            bind_iface="$iface"
            bind_cidr="$source_addr/$(echo "$subnet" | cut -d/ -f2)"
        fi
        if ip -6 addr add "$bind_cidr" dev "$bind_iface" 2>/dev/null; then
            sleep 2
            for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                curl -6 --interface "$source_addr" -fsS --max-time 8 "$endpoint" >/dev/null 2>&1 && connected=1 && break
            done
            ip -6 addr del "$bind_cidr" dev "$bind_iface" 2>/dev/null || true
        fi
    fi
    [ "$connected" = "1" ] || {
        log "$(t "IPv6 source-address connectivity probe failed." "IPv6 独立源地址连通性验证失败。")" >&2
        return 1
    }
    log "$(t "✓ IPv6 values and source-address connectivity verified." "✓ IPv6 参数及独立源地址连通性验证通过。")"
}

prompt_manual_ipv6() {
    local default_iface default_addr default_subnet default_kind answer
    default_iface=$(ip -6 route show default 2>/dev/null | head -1 | awk '{print $5}')
    read -rp "$(t "IPv6 uplink interface [$default_iface]: " "IPv6 上行网卡 [$default_iface]: ")" answer
    IPV6_IFACE="${answer:-$default_iface}"
    default_addr=$(ip -o -6 addr show dev "$IPV6_IFACE" scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
    read -rp "$(t "Host/gateway address from the usable prefix [$default_addr]: " "可用前缀内的宿主机/网关地址 [$default_addr]: ")" answer
    IPV6_ADDR="${answer:-$default_addr}"
    default_subnet=$(python3 - "$IPV6_ADDR" 2>/dev/null <<'PY' || true
import ipaddress, sys
print(ipaddress.IPv6Network((ipaddress.IPv6Address(sys.argv[1]), 64), strict=False))
PY
)
    read -rp "$(t "Usable IPv6 subnet CIDR [$default_subnet]: " "可用 IPv6 子网 CIDR [$default_subnet]: ")" answer
    IPV6_SUBNET="${answer:-$default_subnet}"
    case "$IPV6_IFACE" in he-*|wg*|tun*) default_kind=1 ;; *) default_kind=2 ;; esac
    printf '%s\n  1) %s\n  2) %s\n' \
        "$(t "Prefix delivery type:" "前缀下发类型:")" \
        "$(t "Routed/tunnel prefix (HE, WireGuard, provider route)" "路由/隧道前缀（HE、WireGuard、供应商静态路由）")" \
        "$(t "Native on-link /64 (requires NDP)" "原生二层 /64（需要 NDP）")"
    read -rp "[$default_kind] > " answer
    [ "${answer:-$default_kind}" = "1" ] && IPV6_ROUTED=1 || IPV6_ROUTED=0
    IPV6_MODE="subnet"
}

prompt_ipv6_strategy() {
    local backend="${1:-incus}" choice
    printf '\n%s\n' "$(t "Container network mode:" "容器网络模式:")"
    printf '  1) %s\n' "$(t "NAT4 + public IPv6 (auto detect; recommended)" "NAT4 + 公网 IPv6（自动探测，推荐）")"
    if [ "$backend" = "incus" ]; then
        printf '  2) %s\n' "$(t "IPv6-only (auto detect)" "纯 IPv6（自动探测）")"
        printf '  3) %s\n' "$(t "NAT4 + manual routed/native /64" "NAT4 + 手工路由/原生 /64")"
        printf '  4) %s\n' "$(t "IPv6-only + manual routed/native /64" "纯 IPv6 + 手工路由/原生 /64")"
        printf '  5) %s\n' "$(t "IPv4 NAT only" "仅 IPv4 NAT")"
        printf '  6) %s\n' "$(t "NAT4 + IPv6 SNAT (auto-detect host address)" "NAT4 + IPv6 SNAT（自动探测宿主机地址）")"
    else
        printf '  2) %s\n' "$(t "NAT4 + manual routed/native /64" "NAT4 + 手工路由/原生 /64")"
        printf '  3) %s\n' "$(t "IPv4 NAT only" "仅 IPv4 NAT")"
        printf '  4) %s\n' "$(t "NAT4 + IPv6 SNAT (auto-detect host address)" "NAT4 + IPv6 SNAT（自动探测宿主机地址）")"
    fi
    read -rp '[1] > ' choice
    if [ "$backend" != "incus" ]; then
        case "${choice:-1}" in
            1) IPV6_DETECT_CONFIRMED=1 ;;
            2) prompt_manual_ipv6 ;;
            3) IPV6_MODE="none" ;;
            4) IPV6_MODE="snat"; IPV6_DETECT_CONFIRMED=1 ;;
            *) die "$(t "Invalid network mode." "无效网络模式。")" ;;
        esac
        return 0
    fi
    case "${choice:-1}" in
        1) INCUS_IPV6_ONLY=0; IPV6_DETECT_CONFIRMED=1 ;;
        2) INCUS_IPV6_ONLY=1; IPV6_DETECT_CONFIRMED=1 ;;
        3) INCUS_IPV6_ONLY=0; prompt_manual_ipv6 ;;
        4) INCUS_IPV6_ONLY=1; prompt_manual_ipv6 ;;
        5) INCUS_IPV6_ONLY=0; IPV6_MODE="none" ;;
        6) INCUS_IPV6_ONLY=0; IPV6_MODE="snat"; IPV6_DETECT_CONFIRMED=1 ;;
        *) die "$(t "Invalid network mode." "无效网络模式。")" ;;
    esac
}

# Detect a separately routed prefix commonly used by HE 6in4/WireGuard setups.
# Such hosts have the tunnel link address on the default interface and one
# address from the routed prefix (often /128 on lo) on another interface.  A
# temporary source-address probe proves the candidate before it is selected.
# Returns "SUBNET|HOST_ADDR" or an empty string.
detect_routed_ipv6_subnet() {
    local uplink_iface="$1" uplink_addr="$2"
    local rec cand_iface cand_cidr candidate host_addr test_addr connected

    while IFS='|' read -r cand_iface cand_cidr; do
        [ -n "$cand_iface" ] && [ "$cand_iface" != "$uplink_iface" ] || continue
        rec=$(python3 - "$cand_cidr" "$uplink_addr" <<'PY' 2>/dev/null || true
import ipaddress
import sys

iface = ipaddress.IPv6Interface(sys.argv[1])
uplink = ipaddress.IPv6Address(sys.argv[2])
# Docker/Podman bridges frequently expose ULA prefixes (fc00::/7). They may
# pass a source-address curl probe through host NAT, but they are not provider-
# routed public prefixes and must never replace the uplink's global address.
if not iface.ip.is_global:
    raise SystemExit(0)
# A /128 on lo is the usual routed-prefix gateway marker.  Derive /64 and
# prove it below; false candidates fail the independent-source probe.
prefix = min(iface.network.prefixlen, 64)
network = ipaddress.IPv6Network((iface.ip, prefix), strict=False)
if uplink in network:
    raise SystemExit(0)
test = network.network_address + 0xfffe
print(f"{network}|{iface.ip}|{test}")
PY
)
        [ -n "$rec" ] || continue
        candidate=$(echo "$rec" | cut -d'|' -f1)
        host_addr=$(echo "$rec" | cut -d'|' -f2)
        test_addr=$(echo "$rec" | cut -d'|' -f3)

        connected=0
        if ip -6 addr add "$test_addr/128" dev lo 2>/dev/null; then
            for endpoint in ip.sb ipv6.icanhazip.com www.cloudflare.com; do
                if curl -6 --interface "$test_addr" -s --max-time 8 "$endpoint" >/dev/null 2>&1; then
                    connected=1
                    break
                fi
            done
            ip -6 addr del "$test_addr/128" dev lo 2>/dev/null || true
        fi
        if [ "$connected" = "1" ]; then
            echo "$candidate|$host_addr"
            return 0
        fi
    done < <(ip -o -6 addr show scope global 2>/dev/null | awk '{print $2 "|" $4}')
    return 0
}

# Returns "none" or "IFACE|ADDR|PREFIX|SUBNET|ROUTED".
detect_and_configure_ipv6() {
    log "$(t "Detecting public IPv6..." "开始检测公网 IPv6...")" >&2
    command -v python3 >/dev/null 2>&1 || {
        log "$(t "python3 is required for safe IPv6 prefix arithmetic." "安全计算 IPv6 前缀需要 python3。")" >&2
        echo "none"
        return 1
    }
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

    # Prefer a separately routed prefix over the point-to-point/tunnel link
    # prefix.  No NDP is needed for a routed prefix and the uplink address must
    # keep its original mask so the tunnel gateway remains reachable.
    local routed routed_subnet routed_host routed_prefix
    routed=$(detect_routed_ipv6_subnet "$iface" "$ipv6_addr")
    if [ -n "$routed" ]; then
        routed_subnet=$(echo "$routed" | cut -d'|' -f1)
        routed_host=$(echo "$routed" | cut -d'|' -f2)
        routed_prefix=$(echo "$routed_subnet" | cut -d'/' -f2)
        log "$(t "✓ Routed IPv6 prefix confirmed: $routed_subnet (uplink $iface)." \
            "✓ 已验证独立路由 IPv6 前缀: $routed_subnet（上行 $iface）。")" >&2
        echo "$iface|$routed_host|$routed_prefix|$routed_subnet|1"
        return 0
    fi
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
            echo "$iface|$ipv6_addr|$prefix_len||0"
        else
            log "$(t "⚠ ISP blocks non-assigned source IPs (strict uRPF), falling back to SNAT mode." "⚠ ISP 严格过滤非分配源 IP（strict uRPF），回退到 SNAT 模式。")" >&2
            echo "$iface|$ipv6_addr|128||0"
        fi
    elif [ "$prefix_len" -eq 128 ]; then
        log "$(t "✓ Public IPv6 single address (/128) detected, SNAT mode." "✓ 检测到公网 IPv6 单地址 (/128)，使用 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128||0"
    else
        # /65-/127：前缀不足以划分子网，回退到 SNAT
        log "$(t "IPv6 prefix /$prefix_len is too small for subnet mode (need ≤/64), falling back to SNAT." "IPv6 前缀 /$prefix_len 不足以用于子网模式（需 ≤/64），回退到 SNAT 模式。")" >&2
        echo "$iface|$ipv6_addr|128||0"
    fi
}

if [ "$IPV6_ONESHOT_MODE" = "detect" ]; then
    detect_and_configure_ipv6
    exit 0
elif [ "$IPV6_ONESHOT_MODE" = "validate" ]; then
    if { [ -z "${IPV6_ADDR:-}" ] || [ -z "${IPV6_SUBNET:-}" ] || [ -z "${IPV6_IFACE:-}" ]; } && [ -t 0 ]; then
        prompt_manual_ipv6
    fi
    IPV6_MODE="${IPV6_MODE:-subnet}"
    validate_ipv6_values "$IPV6_MODE" "${IPV6_ADDR:-}" "${IPV6_SUBNET:-}" "${IPV6_IFACE:-}" || exit 1
    probe_ipv6_values "$IPV6_MODE" "${IPV6_ADDR:-}" "${IPV6_SUBNET:-}" "${IPV6_IFACE:-}" "$IPV6_ROUTED" || exit 1
    printf '%s|%s|%s|%s|%s\n' "$IPV6_IFACE" "$IPV6_ADDR" "$(echo "$IPV6_SUBNET" | cut -d/ -f2)" "$IPV6_SUBNET" "$IPV6_ROUTED"
    exit 0
fi

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
        # 在临时文件中完全预分配，失败时不留下半成品，也不再用 dd 继续填满根分区。
        local creating="${disk}.creating"
        rm -f -- "$creating"
        if ! fallocate -l "$size" "$creating" 2>/dev/null; then
            rm -f -- "$creating"
            log "$(t "Unable to allocate $size; choose a smaller data disk." "无法分配 $size，请选择更小的数据盘。")"
            return 1
        fi
        if ! mkfs.xfs -f "$creating"; then
            rm -f -- "$creating"
            return 1
        fi
        mv "$creating" "$disk"
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

recommended_podman_data_size() {
    local avail_mb target
    avail_mb=$(df -Pm / | awk 'NR==2 {print $4}')
    target=$(( (avail_mb - 2048) / 1024 ))
    [ "$target" -gt 20 ] && target=20
    [ "$target" -ge 2 ] || return 1
    printf '%sG\n' "$target"
}

validate_podman_data_size() {
    local value="$1" amount unit requested_mb avail_mb
    case "$value" in
        *[Gg]) amount="${value%[Gg]}"; unit=G ;;
        *[Mm]) amount="${value%[Mm]}"; unit=M ;;
        *) return 1 ;;
    esac
    case "$amount" in ''|*[!0-9]*) return 1 ;; esac
    [ "$amount" -gt 0 ] || return 1
    if [ "$unit" = G ]; then requested_mb=$((amount * 1024)); else requested_mb=$amount; fi
    [ "$requested_mb" -ge 1024 ] || return 1
    avail_mb=$(df -Pm / | awk 'NR==2 {print $4}')
    [ $((requested_mb + 1536)) -le "$avail_mb" ] || {
        log "$(t "Insufficient root filesystem space: requested $value, available ${avail_mb}M; keep at least 1536M free." \
            "根分区空间不足：请求 $value，可用 ${avail_mb}M；安装后至少需保留 1536M。")"
        return 1
    }
}

prompt_podman_options() {
    local recommended choice custom mirror
    recommended=$(recommended_podman_data_size) \
        || die "$(t "At least 3.5 GiB free space is required for a Podman installation." "Podman 安装至少需要约 3.5 GiB 可用空间。")"
    printf '\n%s\n' "$(t "Podman XFS data disk size:" "Podman XFS 数据盘容量:")"
    printf '  1) %s (%s)\n' "$(t "Recommended for current disk" "按当前磁盘推荐")" "$recommended"
    printf '  2) 5G\n  3) 10G\n  4) 20G\n  5) %s\n' "$(t "Custom size" "自定义容量")"
    read -rp '[1] > ' choice
    case "${choice:-1}" in
        1) PODMAN_DATA_SIZE="$recommended" ;;
        2) PODMAN_DATA_SIZE=5G ;;
        3) PODMAN_DATA_SIZE=10G ;;
        4) PODMAN_DATA_SIZE=20G ;;
        5) read -rp "$(t "Custom size (for example 8G): " "自定义容量（例如 8G）: ")" custom; PODMAN_DATA_SIZE="$custom" ;;
        *) die "$(t "Invalid data disk selection." "无效的数据盘选项。")" ;;
    esac
    validate_podman_data_size "$PODMAN_DATA_SIZE" \
        || die "$(t "Invalid or oversized Podman data disk value: $PODMAN_DATA_SIZE" "Podman 数据盘容量无效或超过可用空间: $PODMAN_DATA_SIZE")"
    read -rp "$(t "docker.io registry mirror (blank = direct): " "docker.io 镜像加速地址（留空直连）: ")" mirror
    PODMAN_REGISTRY_MIRROR="${mirror:-$PODMAN_REGISTRY_MIRROR}"
    [ -z "$mirror" ] || PODMAN_REGISTRY_MIRROR_EXPLICIT=1
}

configure_podman_registry_mirror() {
    if [ -z "$PODMAN_REGISTRY_MIRROR" ]; then
        if [ "${PODMAN_REGISTRY_MIRROR_EXPLICIT:-0}" = "1" ]; then
            rm -f -- /etc/containers/registries.conf.d/99-runman-mirror.conf
            log "$(t "Podman docker.io mirror cleared; direct/default registry settings will be used." "已清除 Podman docker.io 镜像加速，将使用直连/系统默认配置。")"
        fi
        return 0
    fi
    local raw="$PODMAN_REGISTRY_MIRROR" location insecure=false
    case "$raw" in
        http://*) insecure=true; location="${raw#http://}" ;;
        https://*) location="${raw#https://}" ;;
        *) location="$raw" ;;
    esac
    location="${location%/}"
    case "$location" in ''|*[!A-Za-z0-9._:/-]*) die "$(t "Invalid Podman registry mirror: $raw" "Podman 镜像加速地址无效: $raw")" ;; esac
    mkdir -p /etc/containers/registries.conf.d
    cat > /etc/containers/registries.conf.d/99-runman-mirror.conf <<EOF
[[registry]]
prefix = "docker.io"
location = "docker.io"

[[registry.mirror]]
location = "$location"
insecure = $insecure
EOF
    log "$(t "Podman docker.io mirror configured: $location" "Podman docker.io 镜像加速已配置: $location")"
}

install_podman_forwarding_compat() {
    # Docker sets the shared iptables FORWARD policy to DROP. Netavark's nft
    # base chain cannot override an earlier DROP, so Podman traffic dies when
    # Docker and Podman coexist. DOCKER-USER is Docker's supported insertion
    # point; limit exceptions strictly to the Runman network subnets.
    cat > /usr/local/sbin/runman-podman-forwarding <<'EOF'
#!/usr/bin/env bash
set -u

apply_rule() {
    local tool="$1"; shift
    "$tool" -w -C DOCKER-USER "$@" 2>/dev/null || "$tool" -w -I DOCKER-USER 1 "$@"
}
delete_rule() {
    local tool="$1"; shift
    while "$tool" -w -C DOCKER-USER "$@" 2>/dev/null; do
        "$tool" -w -D DOCKER-USER "$@" 2>/dev/null || break
    done
}

mode="${1:-apply}"
if iptables -w -S DOCKER-USER >/dev/null 2>&1; then
    if [ "$mode" = remove ]; then
        delete_rule iptables -s 10.91.0.0/20 -j ACCEPT
        delete_rule iptables -d 10.91.0.0/20 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    else
        apply_rule iptables -d 10.91.0.0/20 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
        apply_rule iptables -s 10.91.0.0/20 -j ACCEPT
    fi
fi
if ip6tables -w -S DOCKER-USER >/dev/null 2>&1; then
    if [ "$mode" = remove ]; then
        delete_rule ip6tables -s fd91:cafe:cafe:10::/64 -j ACCEPT
        delete_rule ip6tables -d fd91:cafe:cafe:10::/64 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    else
        apply_rule ip6tables -d fd91:cafe:cafe:10::/64 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
        apply_rule ip6tables -s fd91:cafe:cafe:10::/64 -j ACCEPT
    fi
fi
EOF
    chmod 700 /usr/local/sbin/runman-podman-forwarding
    cat > /etc/systemd/system/runman-podman-forwarding.service <<'EOF'
[Unit]
Description=Runman Podman forwarding compatibility for Docker hosts
After=network-online.target docker.service podman.socket
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/runman-podman-forwarding apply
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
    start_service runman-podman-forwarding
    log "$(t "Podman/Docker forwarding compatibility applied." "已应用 Podman/Docker 共存转发兼容规则。")"
}

download_agent() {
    local arch=$1
    mkdir -p "$AGENT_BIN_DIR"

    # 调试模式：检查本地文件
    if [ "$UPDATE_ONLY" != "1" ] && [ -f "./runman-agent-linux-$arch" ]; then
        log "$(t "Found local agent binary: ./runman-agent-linux-$arch (debug mode)" "找到本地 agent 二进制: ./runman-agent-linux-$arch (调试模式)")"
        cp "./runman-agent-linux-$arch" "$AGENT_BINARY.new"
        chmod +x "$AGENT_BINARY.new"
        [ "${2:-}" = "stage" ] || mv "$AGENT_BINARY.new" "$AGENT_BINARY"
        log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY (from local)" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY (来自本地)")"
        return 0
    fi

    # 生产模式：从远程下载
    log "$(t "Downloading NarwhalCloud Agent ($arch)..." "下载 NarwhalCloud Agent ($arch)...")"
    download_with_retry "$DOWNLOAD_BASE/runman-agent-linux-$arch" "$AGENT_BINARY.new"
    python3 - "$AGENT_BINARY.new" "$arch" <<'PY'
import sys
with open(sys.argv[1], 'rb') as binary:
    header = binary.read(20)
expected = {'amd64': 62, 'arm64': 183}[sys.argv[2]]
if len(header) != 20 or header[:6] != b'\x7fELF\x02\x01' or int.from_bytes(header[18:20], 'little') != expected:
    raise SystemExit('Downloaded Agent is not an ELF64 binary for the selected architecture')
PY
    chmod +x "$AGENT_BINARY.new"
    if [ "${2:-}" = "stage" ]; then
        log "$(t "✓ Agent download staged and architecture checked." "✓ Agent 已下载暂存并通过架构检查。")"
    else
        mv "$AGENT_BINARY.new" "$AGENT_BINARY"
        log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY")"
    fi
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
    local agent_token="${11:-}"

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
    if [ -n "$agent_token" ]; then
        jq --arg token "$agent_token" '.token = $token' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp"
        mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE"
    fi
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
        elif [ "$mode" = "1" ] || [ "$NON_INTERACTIVE" = "1" ]; then
            # --install-rfw promises a no-prompt forced install. Prefer the
            # default IPv4 uplink so tunnel/bridge interfaces are not selected.
            SEL_IFACE=$(ip -4 route show default 2>/dev/null | awk 'NR==1 {print $5}')
            if [ -z "$SEL_IFACE" ] || ! printf '%s\n' "${interfaces[@]}" | grep -Fxq "$SEL_IFACE"; then
                SEL_IFACE="${interfaces[0]}"
            fi
            log "$(t "Non-interactive rfw uplink: $SEL_IFACE" "无交互 rfw 上行网卡: $SEL_IFACE")"
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
        alpine) echo "alpine/$DEFAULT_INCUS_ALPINE_VERSION/cloud/$ARCH/ready" ;;
    esac
}

import_incus_images() {
    local refresh="${INCUS_IMAGE_REFRESH:-0}"
    local stamp_dir="$AGENT_DATA_DIR/incus-image-stamps"
    local requested_distros=("$@")
    [ ${#requested_distros[@]} -gt 0 ] || requested_distros=(debian alpine)
    command -v incus &>/dev/null || return 0
    mkdir -p "$stamp_dir"

    local distro alias asset url tmp stamp_file remote_stamp present
    for distro in "${requested_distros[@]}"; do
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

    MIRROR_IMPORTED_DISTROS=""
    local ok=0 distro item_record metadata_path rootfs_path metadata_sha rootfs_sha tmpd metadata_file rootfs_file local_alias
    for distro in alpine debian; do
        # Accept current Incus names (incus.tar.xz + root.squashfs) and the
        # legacy LXD names. Select the newest complete version deterministically.
        item_record=$(printf '%s\n' "$meta" | jq -r --arg d "$distro" '
          [.products | to_entries[]?
           | select((.key | startswith($d + ":")) or (.value.os? == $d))
           | .value.versions | to_entries[]?
           | {version: .key, items: .value.items}
           | .metadata = (.items["incus.tar.xz"] // .items["lxd.tar.xz"])
           | .rootfs = (.items["root.squashfs"] // .items["rootfs.squashfs"])
           | select(.metadata.path? and .rootfs.path?)]
          | sort_by(.version) | reverse | .[0]
          | [.metadata.path, .rootfs.path, (.metadata.sha256 // ""), (.rootfs.sha256 // "")]
          | @tsv' 2>/dev/null)
        [ -n "$item_record" ] && [ "$item_record" != "null" ] || continue
        IFS=$'\t' read -r metadata_path rootfs_path metadata_sha rootfs_sha <<< "$item_record"
        [ -n "$metadata_path" ] && [ -n "$rootfs_path" ] || continue

        tmpd=$(mktemp -d)
        metadata_file="$tmpd/incus.tar.xz"; rootfs_file="$tmpd/root.squashfs"
        if download_with_retry "$base/$metadata_path" "$metadata_file" && download_with_retry "$base/$rootfs_path" "$rootfs_file"; then
            if { [ -n "$metadata_sha" ] && [ "$(sha256sum "$metadata_file" | awk '{print $1}')" != "$metadata_sha" ]; } \
                || { [ -n "$rootfs_sha" ] && [ "$(sha256sum "$rootfs_file" | awk '{print $1}')" != "$rootfs_sha" ]; }; then
                log "$(t "Warning: mirror checksum verification failed for $distro" "警告: 镜像服务器中的 $distro 校验失败")"
                rm -rf "$tmpd"
                continue
            fi
            local_alias=$(incus_ready_alias "$distro")
            incus image delete "$local_alias" >/dev/null 2>&1 || true
            if incus image import "$metadata_file" "$rootfs_file" --alias "$local_alias"; then
                log "$(t "✓ Imported $distro from mirror as $local_alias" "✓ 已从镜像服务器导入 $distro 为 $local_alias")"
                ok=1
                MIRROR_IMPORTED_DISTROS="$MIRROR_IMPORTED_DISTROS $distro"
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

import_missing_incus_images() {
    local saved_mirror="$INCUS_IMAGE_MIRROR" distro
    INCUS_IMAGE_MIRROR=""
    for distro in debian alpine; do
        case " $MIRROR_IMPORTED_DISTROS " in
            *" $distro "*) ;;
            *) import_incus_images "$distro" ;;
        esac
    done
    INCUS_IMAGE_MIRROR="$saved_mirror"
}

# simplestreams 不可用时，仅当镜像源确实提供扁平 tarball 才按扁平源处理；
# 否则切回官方预构建 Release，避免对一串必然 404 的 URL 重试。
import_incus_fallback_images() {
    local saved_mirror="$INCUS_IMAGE_MIRROR"
    if [ -n "$saved_mirror" ] \
        && curl -fsSI --max-time 10 "${saved_mirror%/}/incus-alpine-${ARCH}.tar.gz" >/dev/null 2>&1; then
        import_incus_images
    else
        INCUS_IMAGE_MIRROR=""
        import_incus_images
        INCUS_IMAGE_MIRROR="$saved_mirror"
    fi
}

# 交互式选择 SSH 登录横幅（欢迎页）预设；已通过参数/环境变量指定时跳过。
prompt_banner() {
    [ "$NON_INTERACTIVE" = "1" ] && return 0
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

prompt_existing_image_update() {
    local virt="$1" choice current value
    if [ "$virt" = "incus" ]; then
        current=$(jq -r '.incus_image_mirror // ""' "$AGENT_CONFIG_FILE" 2>/dev/null || true)
        printf '\n%s\n%s\n' "$(t "Incus image management:" "Incus 镜像管理:")" \
            "$(t "Current mirror: ${current:-<GitHub/default>}" "当前镜像源: ${current:-<GitHub/默认>}")"
        printf '  1) %s\n' "$(t "Refresh from current source" "从当前镜像源强制刷新")"
        printf '  2) %s\n' "$(t "Set a simplestreams/flat mirror URL and refresh" "设置 simplestreams/扁平镜像地址并刷新")"
        printf '  3) %s\n' "$(t "Import from an offline local directory" "从离线本地目录导入")"
        printf '  4) %s\n' "$(t "Set a custom Alpine base file/alias" "设置自定义 Alpine 基础镜像文件/别名")"
        printf '  5) %s\n' "$(t "Reset to the podcctv default mirror" "恢复 podcctv 默认镜像源")"
        read -rp '[1] > ' choice
        case "${choice:-1}" in
            1) INCUS_IMAGE_MIRROR="$current" ;;
            2) read -rp "$(t "Mirror URL: " "镜像服务 URL: ")" value; [ -n "$value" ] || die "$(t "Mirror URL cannot be empty." "镜像服务 URL 不能为空。")"; INCUS_IMAGE_MIRROR="$value"; INCUS_IMAGE_MIRROR_EXPLICIT=1 ;;
            3) read -rp "$(t "Local image directory: " "本地镜像目录: ")" value; [ -d "$value" ] || die "$(t "Directory not found: $value" "目录不存在: $value")"; INCUS_LOCAL_IMAGE_DIR="$value" ;;
            4) read -rp "$(t "Alpine base tarball path or Incus alias: " "Alpine 基础镜像 tarball 路径或 Incus 别名: ")" value; [ -n "$value" ] || die "$(t "Value cannot be empty." "输入不能为空。")"; INCUS_ALPINE_BASE="$value" ;;
            5) INCUS_IMAGE_MIRROR="$DEFAULT_INCUS_IMAGE_MIRROR"; INCUS_IMAGE_MIRROR_EXPLICIT=1 ;;
            *) die "$(t "Invalid image operation." "无效的镜像操作。")" ;;
        esac
    elif [ "$virt" = "podman" ]; then
        printf '\n%s\n' "$(t "Podman image management:" "Podman 镜像管理:")"
        printf '  1) %s\n' "$(t "Refresh the built-in Debian and Alpine images" "刷新内置 Debian 与 Alpine 镜像")"
        printf '  2) %s\n' "$(t "Set docker.io registry mirror and refresh" "设置 docker.io 镜像加速并刷新")"
        printf '  3) %s\n' "$(t "Clear installer-managed registry mirror" "清除安装脚本管理的镜像加速")"
        read -rp '[1] > ' choice
        case "${choice:-1}" in
            1) ;;
            2) read -rp "$(t "Registry mirror (host[:port][/prefix] or URL): " "镜像加速地址（host[:port][/prefix] 或 URL）: ")" value; [ -n "$value" ] || die "$(t "Mirror cannot be empty." "镜像加速地址不能为空。")"; PODMAN_REGISTRY_MIRROR="$value"; PODMAN_REGISTRY_MIRROR_EXPLICIT=1 ;;
            3) PODMAN_REGISTRY_MIRROR=""; PODMAN_REGISTRY_MIRROR_EXPLICIT=1 ;;
            *) die "$(t "Invalid image operation." "无效的镜像操作。")" ;;
        esac
    else
        log "$(t "Image refresh will use the current cloud-hypervisor source." "镜像刷新将使用当前 cloud-hypervisor 镜像源。")"
    fi
}

# ── Update flow ───────────────────────────────────────────────────────────────

# Never interpret an incomplete/custom original installation as a fresh install.
preflight_existing_install() {
    [ "$(id -u)" = "0" ] || die "Update requires root."
    for dep in jq python3 curl flock; do
        command -v "$dep" >/dev/null 2>&1 || die "Update requires $dep; install it first."
    done
    [ -f "$AGENT_BINARY" ] && [ -f "$AGENT_CONFIG_FILE" ] \
        || die "Incomplete/nonstandard installation: expected $AGENT_BINARY and $AGENT_CONFIG_FILE. No fresh installation attempted."
    jq -e 'type == "object"' "$AGENT_CONFIG_FILE" >/dev/null \
        || die "Invalid existing config.json; update aborted."
    local virt command_line
    virt=$(detect_installed_virt_type)
    case "$virt" in podman|incus|cloudhv) ;; *) die "Unknown existing virt_type; update aborted." ;; esac
    [ -z "$VIRT_TYPE_REQUESTED" ] || [ "$VIRT_TYPE_REQUESTED" = "$virt" ] \
        || die "An update cannot switch virtualization backends. Existing backend: $virt"
    [ "$UPDATE_NETWORK_REQUESTED" = "0" ] && [ -z "${IPV6_MODE:-}" ] \
        && [ -z "${IPV6_SUBNET:-}" ] && [ -z "${IPV6_ADDR:-}" ] && [ -z "${IPV6_IFACE:-}" ] \
        || die "An update preserves existing networking; configure network changes separately."
    if [ "$UPDATE_ONLY" = "1" ]; then
        [ "$IMAGE_MENU" = "0" ] && [ "$FORCE_IMAGE_REFRESH" != "1" ] \
            && [ "$INSTALL_RFW_FORCE" != "1" ] && [ -z "$INITIAL_TOKEN" ] \
            && [ "$INCUS_IMAGE_MIRROR_EXPLICIT" != "1" ] && [ -z "$INCUS_ALPINE_BASE" ] \
            && [ -z "$INCUS_LOCAL_IMAGE_DIR" ] && [ "$PODMAN_REGISTRY_MIRROR_EXPLICIT" != "1" ] \
            || die "--update-only cannot be combined with Token, image or firewall changes."
    fi
    command_line=$(systemctl show "$AGENT_SERVICE" -p ExecStart --value)
    [[ "$command_line" == *"$AGENT_BINARY"* && "$command_line" == *"$AGENT_CONFIG_FILE"* ]] \
        || die "Nonstandard service ExecStart/config path; manual migration is required."
    AGENT_DB=$(jq -r '.db // empty' "$AGENT_CONFIG_FILE")
    AGENT_DB="${AGENT_DB:-$AGENT_CONFIG_DIR/agent.db}"
    [[ "$AGENT_DB" = /* ]] && [ -f "$AGENT_DB" ] \
        || die "Existing database missing or not an absolute path: $AGENT_DB; update aborted."
}

backup_existing_install() {
    mkdir -p "$INCUS_IPV6_BACKUP_DIR"
    UPGRADE_BACKUP=$(mktemp -d "$INCUS_IPV6_BACKUP_DIR/upgrade-$(date +%Y%m%d-%H%M%S)-XXXXXX")
    chmod 700 "$UPGRADE_BACKUP"
    cp -p "$AGENT_BINARY" "$UPGRADE_BACKUP/narwhal-agent"
    cp -p "$AGENT_CONFIG_FILE" "$UPGRADE_BACKUP/config.json"
    systemctl cat "$AGENT_SERVICE" > "$UPGRADE_BACKUP/service.txt"
    # SQLite's online backup API includes committed WAL data; copying a live
    # agent.db alone can silently lose VM records and port-forward rules.
    python3 - "$AGENT_DB" "$UPGRADE_BACKUP/agent.db" <<'PY'
import pathlib
import sqlite3
import sys
with sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri() + '?mode=ro', uri=True, timeout=30) as source:
    with sqlite3.connect(sys.argv[2]) as target:
        source.backup(target)
        if target.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
            raise RuntimeError('Database backup failed integrity check')
PY
    chmod 600 "$UPGRADE_BACKUP/config.json" "$UPGRADE_BACKUP/agent.db" "$UPGRADE_BACKUP/service.txt"
    log "$(t "Pre-upgrade backup (keep private):" "升级前备份（请妥善保密）:") $UPGRADE_BACKUP"
}

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ] || [ -f "$AGENT_CONFIG_FILE" ]; then
    log "$(t "Existing NarwhalCloud Agent detected, updating..." "检测到已安装的 NarwhalCloud Agent，执行更新流程...")"
    preflight_existing_install
    exec 9>/run/lock/narwhal-agent-install.lock
    flock -n 9 || die "Another Agent installation/update is already running."
    backup_existing_install
    ARCH=$(detect_arch)
    # Finish downloading before changing config or replacing the running binary.
    download_agent "$ARCH" stage

    _installed_virt=$(detect_installed_virt_type)
    if [ "$IMAGE_MENU" = "1" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        prompt_existing_image_update "$_installed_virt"
    fi

    if [ -n "$INITIAL_TOKEN" ]; then
        write_agent_token "$INITIAL_TOKEN"
        log "$(t "✓ Integration Token updated from command line/environment." "✓ 已通过命令行/环境变量更新对接 Token。")"
    fi

    # 配置迁移：为旧配置补齐新增字段（保留已有值，仅补充缺省），避免升级后新功能无法生效
    if command -v jq >/dev/null 2>&1 && [ -f "$AGENT_CONFIG_FILE" ]; then
        jq '. + {
               incus_banner_preset: (.incus_banner_preset // "none"),
               incus_banner_text:   (.incus_banner_text   // ""),
               incus_ipv6_alloc:    (.incus_ipv6_alloc    // 1),
               incus_alpine_base:   (.incus_alpine_base   // ""),
               incus_image_mirror:  (.incus_image_mirror  // ""),
               incus_ipv6_only:     (.incus_ipv6_only     // false),
               ipv6_wg_subnet:      (if ((.ipv6_wg_subnet // "") == "" and (.ipv6_mode // "") == "subnet")
                                        then (.ipv6_subnet // "") else (.ipv6_wg_subnet // "") end),
               ipv6_backup_dir:     (.ipv6_backup_dir     // "/var/lib/narwhal-agent/backups")
             }' "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp" \
            && mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE" \
            && chmod 600 "$AGENT_CONFIG_FILE"
    fi

    ARCH=$(detect_arch)
    if [ "$UPDATE_ONLY" != "1" ] && [ "$_installed_virt" = "podman" ]; then
        check_podman_version
    fi

    # 重新应用 sysctl，确保旧安装升级后也获得最新内核参数（forwarding/accept_ra 等）
    if [ "$UPDATE_ONLY" != "1" ]; then
        enable_bbr
        configure_journald
    fi

    mv "$AGENT_BINARY.new" "$AGENT_BINARY"

    # Update netavark
    if [ "$UPDATE_ONLY" != "1" ] && [ "$_installed_virt" = "podman" ] && [ -f "/usr/libexec/podman/netavark" ]; then
        if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark.new"; then
            chmod +x /usr/libexec/podman/netavark.new
            mv /usr/libexec/podman/netavark.new /usr/libexec/podman/netavark
            log "$(t "✓ Custom netavark updated." "✓ 自定义 netavark 已更新。")"
        fi
    fi

    if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null; then
        systemctl restart "$AGENT_SERVICE" \
            || die "Agent restart failed. Backup: $UPGRADE_BACKUP; inspect journalctl -u $AGENT_SERVICE"
        sleep 3
        systemctl is-active --quiet "$AGENT_SERVICE" \
            || die "Agent did not remain active. Backup: $UPGRADE_BACKUP; inspect journalctl -u $AGENT_SERVICE"
    else
        log "$(t "Agent was stopped; leaving it stopped." "Agent 原本已停止，本次保持停止状态。")"
    fi
    log "$(t "✓ NarwhalCloud Agent updated." "✓ NarwhalCloud Agent 已更新。")"
    if [ "$UPDATE_ONLY" = "1" ]; then
        log "$(t "Agent-only update complete; backend, images and host networking unchanged." "仅 Agent 更新完成；后端、镜像与宿主机网络未改动。")"
        exit 0
    fi

    # Refresh prebuilt base images for VM-style virtualization.
    # 已运行的实例使用的是镜像副本，替换基础镜像只影响之后新建的实例。
    VIRT_TYPE="$_installed_virt"
    case "$VIRT_TYPE" in
        podman)
            ensure_podman_network_from_config
            configure_podman_registry_mirror
            install_podman_forwarding_compat
            if [ "$FORCE_IMAGE_REFRESH" = "1" ]; then
                for image in "docker.io/narwhalcloud/debian:podman" "docker.io/narwhalcloud/alpine:podman"; do
                    podman pull "$image" || die "$(t "Failed to refresh Podman image: $image" "Podman 镜像刷新失败: $image")"
                done
            fi
            ;;
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
            if [ "$INCUS_IMAGE_MIRROR_EXPLICIT" = "1" ] || [ -n "$INCUS_ALPINE_BASE" ]; then
                jq --arg mirror "$INCUS_IMAGE_MIRROR" --arg base "$INCUS_ALPINE_BASE" \
                    '.incus_image_mirror = $mirror | if $base != "" then .incus_alpine_base = $base else . end' \
                    "$AGENT_CONFIG_FILE" > "$AGENT_CONFIG_FILE.tmp" \
                    && mv "$AGENT_CONFIG_FILE.tmp" "$AGENT_CONFIG_FILE" \
                    && chmod 600 "$AGENT_CONFIG_FILE"
                systemctl restart "$AGENT_SERVICE"
            fi
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
                import_missing_incus_images
            else
                import_incus_fallback_images
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

[ "$UPDATE_ONLY" != "1" ] || die "No standard Agent installation found; --update-only will not perform a fresh installation."
[ "$IMAGE_MENU" = "1" ] && die "$(t "Agent is not installed; use menu option 1 first." "Agent 尚未安装，请先使用菜单选项 1 安装。")"

log "$(t "Starting fresh installation..." "开始全新安装流程...")"


# ── Virtualization type selection ──────────────────────────────────────────────

if [ -n "$VIRT_TYPE_REQUESTED" ]; then
    VIRT_TYPE="$VIRT_TYPE_REQUESTED"
else
    printf "$(t "Select virtualization type:" "选择虚拟化类型：")\n"
    printf "  1) Podman container ($(t "recommended" "推荐"))\n"
    printf "  2) cloud-hypervisor VM ($(t "experimental, requires /dev/kvm" "实验性，需要 /dev/kvm"))\n"
    printf "  3) Incus (LXC) ($(t "enhanced in this fork" "本 Fork 已增强"))\n"
    log "$(t "WARNING: cloud-hypervisor (type 2) is experimental. Incus has guided image and IPv6 support in this fork." "提示：cloud-hypervisor（选项 2）仍为实验性；本 Fork 已为 Incus 补齐镜像与 IPv6 引导。")"
    read -rp "> " _virt_choice
    case "${_virt_choice}" in
        2) VIRT_TYPE="cloudhv" ;;
        3) VIRT_TYPE="incus" ;;
        *) VIRT_TYPE="podman" ;;
    esac
fi
if [ "$VIRT_TYPE" = "cloudhv" ]; then
    if [ ! -e "/dev/kvm" ]; then
        die "$(t "ERROR: /dev/kvm not found. KVM is required for cloud-hypervisor." "错误: 未找到 /dev/kvm。cloud-hypervisor 需要 KVM 支持。")"
    fi
    log "$(t "✓ KVM support detected." "✓ 检测到 KVM 支持。")"
fi
log "$(t "Selected virtualization type: $VIRT_TYPE" "选择的虚拟化类型: $VIRT_TYPE")"

if [ "$VIRT_TYPE" = "podman" ]; then
    if [ ! -f /xfs_disk.img ]; then
        if [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
            prompt_podman_options
        else
            [ -n "$PODMAN_DATA_SIZE" ] || PODMAN_DATA_SIZE=$(recommended_podman_data_size) \
                || die "$(t "At least 3.5 GiB free space is required for a Podman installation." "Podman 安装至少需要约 3.5 GiB 可用空间。")"
            validate_podman_data_size "$PODMAN_DATA_SIZE" \
                || die "$(t "Invalid or oversized Podman data disk value: $PODMAN_DATA_SIZE" "Podman 数据盘容量无效或超过可用空间: $PODMAN_DATA_SIZE")"
        fi
    elif [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
        read -rp "$(t "docker.io registry mirror (blank = keep direct/default): " "docker.io 镜像加速地址（留空保持直连/现有配置）: ")" _podman_mirror
        [ -z "$_podman_mirror" ] || PODMAN_REGISTRY_MIRROR="$_podman_mirror"
    fi
fi

# ── Installation ───────────────────────────────────────────────────────────────

install_packages "$VIRT_TYPE"
# 在任何受管系统文件变更之前保存唯一的安装基线。后续手工 --backup-ipv6
# 只更新 latest，不覆盖 install-origin，确保 --uninstall 永远恢复到安装前。
if [ ! -s "$INCUS_IPV6_BACKUP_DIR/install-origin" ]; then
    if b=$(backup_ipv6_config); then
        echo "$b" > "$INCUS_IPV6_BACKUP_DIR/install-origin"
        log "$(t "System config baseline saved to $b" "系统配置安装前基线已保存至 $b")"
    fi
fi
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
        # 增加 noatime 优化性能和稳定性；容量已在菜单/参数阶段校验。
        create_xfs_disk "/xfs_disk.img" "$PODMAN_DATA_SIZE" "/data" "defaults,pquota,loop,noatime" \
            || die "$(t "Failed to create the Podman XFS data disk." "Podman XFS 数据盘创建失败。")"
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
    configure_podman_registry_mirror
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
# 三种后端的交互安装统一进入网络场景菜单；命令行已给 --ipv6-mode 时不再询问。
# Podman/cloud-hypervisor 固定保留 NAT4，因此不会显示 Incus 专属的纯 IPv6 选项。
if [ "$NON_INTERACTIVE" != "1" ] && [ -z "${IPV6_MODE:-}" ]; then
    prompt_ipv6_strategy "$VIRT_TYPE"
fi

# 保存用户通过环境变量显式指定的值（如 IPV6_MODE=subnet bash install.sh）
_USER_IPV6_MODE="${IPV6_MODE:-}"
IPV6_MODE="none"
MANUAL_IPV6_CONFIG=0

IPV6_CONFIG="none"

if [ "$_USER_IPV6_MODE" = "none" ]; then
    log "$(t "IPv6 mode pre-set to 'none', skipping detection." "IPv6 模式已预设为 'none'，跳过 IPv6 检测。")"
elif [ "$_USER_IPV6_MODE" = "snat" ] && [ -n "${IPV6_ADDR:-}" ] && [ -n "${IPV6_IFACE:-}" ]; then
    MANUAL_IPV6_CONFIG=1
    log "$(t "Manual IPv6 SNAT values provided, validating..." "已提供手工 IPv6 SNAT 参数，开始验证...")"
elif [ "$_USER_IPV6_MODE" = "subnet" ] && [ -n "${IPV6_ADDR:-}" ] && [ -n "${IPV6_SUBNET:-}" ] && [ -n "${IPV6_IFACE:-}" ]; then
    # 用户提供了完整的自定义配置（模式 + 地址 + 子网），跳过检测
    MANUAL_IPV6_CONFIG=1
    log "$(t "Custom IPv6 config provided (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET), skipping detection." \
        "已提供自定义 IPv6 配置 (mode=$_USER_IPV6_MODE addr=$IPV6_ADDR subnet=$IPV6_SUBNET)，跳过检测。")"
elif [ -n "$_USER_IPV6_MODE" ]; then
    log "$(t "IPv6 mode pre-set to '$_USER_IPV6_MODE', auto-detecting IPv6..." "IPv6 模式已预设为 '$_USER_IPV6_MODE'，自动检测 IPv6...")"
    IPV6_CONFIG=$(detect_and_configure_ipv6)
else
    if [ "$NON_INTERACTIVE" = "1" ] || [ "${IPV6_DETECT_CONFIRMED:-0}" = "1" ]; then
        _ipv6=Y
    else
        read -rp "$(t "Enable public IPv6 detection? [Y/n]: " "是否进行公网 IPv6 检测? [Y/n]: ")" _ipv6
    fi
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
    [ -z "$IPV6_SUBNET" ] && IPV6_SUBNET=$(echo "$IPV6_CONFIG" | cut -d'|' -f4)
    [ "$IPV6_ROUTED" = "1" ] || IPV6_ROUTED=$(echo "$IPV6_CONFIG" | cut -d'|' -f5)
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
        if [ -n "$IPV6_IFACE" ] && [ "$IPV6_ROUTED" != "1" ]; then
            configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"
        fi
        log "$(t "✓ IPv6 subnet mode: $IPV6_SUBNET, VMs will get independent addresses." "✓ IPv6 子网模式: $IPV6_SUBNET，VM 将获得独立地址。")"
    elif [ "$IPV6_MODE" = "snat" ]; then
        log "$(t "✓ IPv6 SNAT mode: VMs share host IPv6 via masquerade." "✓ IPv6 SNAT 模式：VM 通过宿主机 MASQUERADE 共享 IPv6。")"
    fi

    # Incus 网桥会在安装末尾按这里解析出的最终模式创建/收敛。
    log "$(t "✓ $VIRT_TYPE network parameters validated." "✓ $VIRT_TYPE 网络参数已验证。")"

    if [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ] && [ -n "$IPV6_IFACE" ]; then
        if [ "$IPV6_ROUTED" = "1" ]; then
            log "$(t "✓ Routed prefix mode: upstream NDP is not required." "✓ 独立路由前缀模式：无需上游 NDP 应答。")"
        else
            log "$(t "✓ NDP responder will be enabled for IPv6 subnet mode." "✓ NDP 响应器将在 IPv6 子网模式下启用。")"
        fi
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

    if [ -n "$IPV6_IFACE" ] && [ "$IPV6_ROUTED" != "1" ]; then
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

install_podman_forwarding_compat

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
if [ "$IPV6_MODE" = "subnet" ] && [ "$IPV6_ROUTED" != "1" ]; then
    if [ "$VIRT_TYPE" = "cloudhv" ] || [ "$VIRT_TYPE" = "incus" ]; then
        NDP_IFACE="$IPV6_IFACE"
        NDP_SUBNETS="$IPV6_SUBNET"
    fi
fi

# 在写 config.json 前确定 WireGuard IPv6 池，确保首启 Agent 即可读取。
if [ "$VIRT_TYPE" = "incus" ] && [ "$IPV6_MODE" = "subnet" ] \
    && [ -n "$IPV6_SUBNET" ] && [ -z "$INCUS_WG_IPV6_SUBNET" ]; then
    INCUS_WG_IPV6_SUBNET="$IPV6_SUBNET"
fi

# 生成默认 Web 面板用户名和随机密码
DEFAULT_USER="admin"
DEFAULT_PASS=$(tr -dc A-Za-z0-9 </dev/urandom | head -c 12 2>/dev/null || openssl rand -base64 9 | tr -dc A-Za-z0-9 | head -c 12)

# 首次安装必须生成或接收一个可轮换的对接 Token。交互输入不回显；留空自动生成。
if [ "$GENERATE_TOKEN" = "1" ]; then
    INITIAL_TOKEN=$(generate_agent_token)
elif [ -z "$INITIAL_TOKEN" ] && [ "$NON_INTERACTIVE" != "1" ] && [ -t 0 ]; then
    read -rsp "$(t "Integration Token (leave blank to generate): " "对接 Token（留空自动生成）: ")" INITIAL_TOKEN
    echo
fi
[ -n "$INITIAL_TOKEN" ] || INITIAL_TOKEN=$(generate_agent_token)
validate_agent_token "$INITIAL_TOKEN" || die "$(t "Invalid integration Token." "无效的对接 Token。")"

# 生成配置文件（包含所有启动参数、IPv6配置和磁盘限制）
write_config_file "$VIRT_TYPE" "$IPV6_MODE" "$IPV6_SUBNET" "$IPV6_ADDR" "$IPV6_IFACE" "$NDP_IFACE" "$NDP_SUBNETS" "$NDP_NETWORK" "$DEFAULT_USER" "$DEFAULT_PASS" "$INITIAL_TOKEN"

write_service_file "$VIRT_TYPE"
start_service "$AGENT_SERVICE"

# ── Optional rfw ─────────────────────────────────────────────────────────────

if [ "$INSTALL_RFW_FORCE" = "1" ]; then
    install_rfw 1
elif [ "$NON_INTERACTIVE" = "1" ]; then
    log "$(t "Non-interactive mode: skipping optional rfw installation." "非交互模式：跳过可选 rfw 安装。")"
else
    install_rfw 0
fi

if [ "$MANUAL_IPV6_CONFIG" = "1" ]; then
    validate_ipv6_values "$IPV6_MODE" "$IPV6_ADDR" "$IPV6_SUBNET" "$IPV6_IFACE" \
        || die "$(t "Manual IPv6 validation failed." "手工 IPv6 参数验证失败。")"
    probe_ipv6_values "$IPV6_MODE" "$IPV6_ADDR" "$IPV6_SUBNET" "$IPV6_IFACE" "$IPV6_ROUTED" \
        || die "$(t "Manual IPv6 online verification failed; use --skip-ipv6-probe only when validation is intentionally offline." \
            "手工 IPv6 在线验证失败；仅在明确离线验证时使用 --skip-ipv6-probe。")"
fi

# ── Done ──────────────────────────────────────────────────────────────────────

if [ "$VIRT_TYPE" = "incus" ]; then
    # 1. 初始化默认存储池
    if ! incus storage show default >/dev/null 2>&1; then
        log "$(t "Initializing Incus default storage pool..." "初始化 Incus 默认存储池...")"
        incus storage create default dir
    fi

    # 2. 创建或收敛默认网桥。纯 IPv6 模式必须同时关闭 IPv4 地址和 NAT，
    # 避免安装结果与面板声明不一致。
    INCUS_IPV4="10.91.0.1/20"
    INCUS_IPV4_NAT="true"
    INCUS_IPV6="none"
    INCUS_IPV6_NAT="false"
    INCUS_DNS="1.1.1.1,2606:4700:4700::1111"
    if [ "${INCUS_IPV6_ONLY:-0}" = "1" ]; then
        INCUS_IPV4="none"
        INCUS_IPV4_NAT="false"
        INCUS_DNS="2606:4700:4700::1111,2001:4860:4860::8888"
    fi

    if [ "$IPV6_MODE" = "snat" ]; then
        INCUS_IPV6="fd91:cafe:cafe:10::1/64"
        INCUS_IPV6_NAT="true"
    elif [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ]; then
        # 使用所分配前缀第一个 /112 的末地址作为网关，避免与常见的
        # routed-prefix 宿主机地址 ::1/128 冲突；容器地址从 ::2 起分配。
        INCUS_IPV6=$(python3 - "$IPV6_SUBNET" <<'PY'
import ipaddress
import sys
net = ipaddress.IPv6Network(sys.argv[1], strict=False)
bridge_prefix = max(net.prefixlen, 112)
bridge = next(net.subnets(new_prefix=bridge_prefix)) if net.prefixlen < bridge_prefix else net
print(f"{bridge.broadcast_address}/{bridge_prefix}")
PY
)
    fi

    if ! incus network show incusbr0 >/dev/null 2>&1; then
        log "$(t "Initializing Incus network bridge incusbr0..." "初始化 Incus 网桥 incusbr0...")"
        incus network create incusbr0 \
            ipv4.address="$INCUS_IPV4" ipv4.nat="$INCUS_IPV4_NAT" \
            ipv6.address="$INCUS_IPV6" ipv6.nat="$INCUS_IPV6_NAT" \
            ipv6.routing=true \
            ipv6.dhcp=false \
            dns.nameservers="$INCUS_DNS"
    else
        log "$(t "Reconciling existing Incus network bridge incusbr0..." "收敛现有 Incus 网桥 incusbr0 配置...")"
        incus network set incusbr0 ipv4.address "$INCUS_IPV4"
        incus network set incusbr0 ipv4.nat "$INCUS_IPV4_NAT"
        incus network set incusbr0 ipv6.address "$INCUS_IPV6"
        incus network set incusbr0 ipv6.nat "$INCUS_IPV6_NAT"
        incus network set incusbr0 ipv6.routing true
        incus network set incusbr0 ipv6.dhcp false
        incus network set incusbr0 dns.nameservers "$INCUS_DNS"
    fi

    [ "$(incus network get incusbr0 ipv4.address)" = "$INCUS_IPV4" ] \
        || die "$(t "Incus IPv4 bridge validation failed." "Incus 网桥 IPv4 配置校验失败。")"
    [ "$(incus network get incusbr0 ipv6.address)" = "$INCUS_IPV6" ] \
        || die "$(t "Incus IPv6 bridge validation failed." "Incus 网桥 IPv6 配置校验失败。")"
    log "$(t "✓ Incus bridge matches the selected network mode." "✓ Incus 网桥与所选网络模式一致。")"

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
            import_missing_incus_images
        else
            import_incus_fallback_images
        fi
    fi

    # 4.1 注册私有 simplestreams 镜像服务器为 incus remote（best-effort）。
    #     既能让运维直接 `incus launch podcctv-mirror:alpine/3.24`，也顺带
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
log "$(t "Token:        $INITIAL_TOKEN" "对接 Token:    $INITIAL_TOKEN")"
echo ""
log "$(t "Token rotation: bash install.sh --rotate-token" "Token 轮换:    bash install.sh --rotate-token")"
log "$(t "Custom Token:  bash install.sh --rotate-token --token '<new-token>'" "自定义 Token:   bash install.sh --rotate-token --token '<新Token>'")"
if [ "${INCUS_IPV6_ONLY:-0}" = "1" ]; then
    _nat4_en="off"; _nat4_zh="关闭"
else
    _nat4_en="on"; _nat4_zh="开启"
fi
log "$(t "Network:      IPv4 NAT=$_nat4_en, IPv6=$IPV6_MODE" "网络模式:      IPv4 NAT=$_nat4_zh, IPv6=$IPV6_MODE")"
log "========================================"
