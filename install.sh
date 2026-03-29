#!/bin/bash
set -e

# ────────────────────────────────────────────────────────────────────────────────
# runman-agent 安装 / 更新脚本
# ────────────────────────────────────────────────────────────────────────────────

AGENT_BINARY="/usr/local/bin/runman-agent"
AGENT_SERVICE="runman-agent"
AGENT_DATA_DIR="/var/lib/runman-agent"
AGENT_DB="$AGENT_DATA_DIR/agent.db"
AGENT_WEB_PORT="8792"
PODMAN_NETWORK="narwhal-net"

DOWNLOAD_BASE="https://github.com/narwhal-cloud/runman-agent/releases/latest/download"
NETAVARK_BASE="https://github.com/narwhal-cloud/netavark/releases/latest/download"
RFW_BASE="https://github.com/narwhal-cloud/rfw/releases/latest/download"

# ────────────────────────────────────────────────────────────────────────────────
# 工具函数
# ────────────────────────────────────────────────────────────────────────────────

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

cleanup_locks() {
    killall apt apt-get dpkg 2>/dev/null || true
    rm -f /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
    dpkg --configure -a 2>/dev/null || true
}

install_packages() {
    local max=3 attempt=1
    while [ $attempt -le $max ]; do
        log "安装软件包 (第 $attempt 次)..."
        if apt-get update -qq && apt-get install -y curl xfsprogs lxcfs uuid-runtime systemd-zram-generator jq sipcalc podman; then
            return 0
        fi
        cleanup_locks
        [ $attempt -eq $max ] && { log "软件包安装失败"; exit 1; }
        attempt=$((attempt + 1))
        sleep 5
    done
}

start_service() {
    systemctl daemon-reload
    local svc=$1
    systemctl is-active --quiet "$svc" || systemctl start "$svc"
    systemctl is-enabled --quiet "$svc" || systemctl enable "$svc"
}

download_with_retry() {
    local url=$1 dest=$2 max=3 attempt=1
    while [ $attempt -le $max ]; do
        log "下载 $url (第 $attempt 次)..."
        if curl -fsSL -o "$dest" "$url"; then
            return 0
        fi
        [ $attempt -eq $max ] && { log "下载失败"; return 1; }
        attempt=$((attempt + 1))
        sleep 5
    done
}

detect_arch() {
    case $(uname -m) in
        x86_64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) log "不支持的架构: $(uname -m)"; exit 1 ;;
    esac
}

# ────────────────────────────────────────────────────────────────────────────────
# IPv6 辅助
# ────────────────────────────────────────────────────────────────────────────────

expand_ipv6() {
    local ip=$1
    local expanded
    expanded=$(sipcalc "$ip" 2>/dev/null | grep "Expanded Address" | awk '{print $4}')
    if [ -n "$expanded" ]; then
        echo "$expanded"
    else
        echo "$ip"
    fi
}

ipv6_plus_one() {
    local ip=$1
    if command -v python3 &>/dev/null; then
        python3 -c "import ipaddress; print(str(ipaddress.IPv6Address('$ip') + 1))" 2>/dev/null && return 0
    fi
    local expanded
    expanded=$(expand_ipv6 "$ip")
    IFS=':' read -r -a parts <<< "$expanded"
    local dec=()
    for part in "${parts[@]}"; do dec+=($((0x$part))); done
    for ((i=7; i>=0; i--)); do
        dec[i]=$((dec[i] + 1))
        [ ${dec[i]} -lt 65536 ] && break
        dec[i]=0
    done
    local result=""
    for part in "${dec[@]}"; do result+=$(printf "%x:" "$part"); done
    echo "${result%:}"
}

# 返回 "local" 或 "IFACE|ADDR|PREFIX"
detect_and_configure_ipv6() {
    log "开始检测公网 IPv6..." >&2
    if ! curl -6 -s --max-time 10 ip.sb >/dev/null 2>&1; then
        log "未检测到公网 IPv6，将使用本地网段" >&2
        echo "local"; return 0
    fi
    log "✓ 公网 IPv6 可用" >&2

    local iface
    iface=$(ip -6 route show default 2>/dev/null | head -1 | awk '{print $5}')
    [ -z "$iface" ] && { log "未找到默认 IPv6 路由" >&2; echo "local"; return 0; }
    log "默认 IPv6 网卡: $iface" >&2

    local ipv6_full ipv6_addr prefix_len
    ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null | grep -v "fe80" | grep "scope global" | head -1 | awk '{print $2}')
    [ -z "$ipv6_full" ] && { log "未找到全局 IPv6 地址" >&2; echo "local"; return 0; }

    ipv6_addr=$(echo "$ipv6_full" | cut -d'/' -f1)
    prefix_len=$(echo "$ipv6_full" | cut -d'/' -f2)
    log "IPv6 地址: $ipv6_addr/$prefix_len" >&2

    [ "$prefix_len" -gt 112 ] && { log "前缀 /$prefix_len 太短，无法分配子网" >&2; echo "local"; return 0; }

    local test_addr
    test_addr=$(ipv6_plus_one "$ipv6_addr")
    log "测试地址: $test_addr/$prefix_len" >&2

    ip addr add "$test_addr/$prefix_len" dev "$iface" 2>/dev/null || { echo "local"; return 0; }
    sleep 3
    local ok=0
    curl -6 --interface "$test_addr" -s --max-time 10 ip.sb >/dev/null 2>&1 && ok=1
    ip addr del "$test_addr/$prefix_len" dev "$iface" 2>/dev/null

    if [ $ok -eq 1 ]; then
        log "✓ 公网 IPv6 测试成功" >&2
        echo "$iface|$ipv6_addr|$prefix_len"
    else
        log "公网 IPv6 测试失败，使用本地网段" >&2
        echo "local"
    fi
}

# ────────────────────────────────────────────────────────────────────────────────
# 核心功能
# ────────────────────────────────────────────────────────────────────────────────

enable_bbr() {
    log "配置 BBR..."
    cat > /etc/sysctl.d/99-runman.conf <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
fs.inotify.max_user_instances=524288
fs.inotify.max_user_watches=1048576
fs.inotify.max_queued_events=65536
kernel.pid_max=4194304
fs.file-max=2097152
fs.nr_open=2097152
vm.swappiness=100
EOF
    sysctl -p /etc/sysctl.d/99-runman.conf >/dev/null 2>&1 && log "✓ BBR 已配置" || log "警告: BBR 配置将在重启后生效"
}

create_xfs_disk() {
    local disk=$1 size=$2 mount=$3 opts=$4
    if [ ! -f "$disk" ]; then
        log "创建 XFS 磁盘 $disk ($size)..."
        fallocate -l "$size" "$disk"
        mkfs.xfs "$disk"
    else
        log "$disk 已存在，跳过"
    fi
    mkdir -p "$mount"
    mountpoint -q "$mount" || mount -o "$opts" "$disk" "$mount"
    grep -q "$disk" /etc/fstab || echo "$disk $mount xfs $opts 0 0" >> /etc/fstab
}

download_agent() {
    local arch=$1
    local url="$DOWNLOAD_BASE/runman-agent-linux-$arch"
    log "下载 runman-agent ($arch)..."
    download_with_retry "$url" "$AGENT_BINARY.new"
    chmod +x "$AGENT_BINARY.new"
    mv "$AGENT_BINARY.new" "$AGENT_BINARY"
    log "✓ runman-agent 已安装到 $AGENT_BINARY"
}

# 写入 systemd service 文件
# 参数: $1=server_addr  $2=token  $3=额外 ndp flags（可选）
write_service_file() {
    local server_addr=$1 token=$2 extra_flags="${3:-}"
    mkdir -p "$AGENT_DATA_DIR"
    cat > "/etc/systemd/system/$AGENT_SERVICE.service" <<EOF
[Unit]
Description=Runman Agent
After=network-online.target podman.socket
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=$AGENT_BINARY \\
  --db $AGENT_DB \\
  --web :$AGENT_WEB_PORT \\
  --socket unix:///run/podman/podman.sock \\
  --type podman \\
  --server $server_addr \\
  --token $token${extra_flags}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# ────────────────────────────────────────────────────────────────────────────────
# 更新流程
# ────────────────────────────────────────────────────────────────────────────────

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ]; then
    log "检测到已安装的 runman-agent，执行更新流程..."

    ARCH=$(detect_arch)
    download_agent "$ARCH"

    if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null; then
        log "重启服务..."
        systemctl restart "$AGENT_SERVICE"
    fi

    # 同步更新 rfw（如已安装）
    if [ -f "/root/rfw/rfw" ] && [ -f "/etc/systemd/system/rfw.service" ]; then
        log "检测到 rfw，开始更新..."
        EXISTING_IFACE=$(grep "ExecStart=" /etc/systemd/system/rfw.service | grep -oP '(?<=--iface )[^ ]+' || true)
        if [ -n "$EXISTING_IFACE" ]; then
            RFW_ARCH=$(uname -m)
            if download_with_retry \
                "$RFW_BASE/rfw-$RFW_ARCH-unknown-linux-musl" \
                "/root/rfw/rfw.new"; then
                chmod +x /root/rfw/rfw.new
                systemctl is-active --quiet rfw && systemctl stop rfw
                mv /root/rfw/rfw.new /root/rfw/rfw
                sed -i "s|^ExecStart=.*|ExecStart=/root/rfw/rfw --iface $EXISTING_IFACE --block-email --block-http --block-socks5 --block-fet-strict --block-wireguard --countries CN|" \
                    /etc/systemd/system/rfw.service
                systemctl daemon-reload
                systemctl start rfw
                log "✓ rfw 已更新"
            fi
        fi
    fi

    IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "获取失败")
    log "========================================"
    log "✓ 更新完成！"
    log "IP: $IP"
    log "========================================"
    exit 0
fi

# ────────────────────────────────────────────────────────────────────────────────
# 全新安装流程
# ────────────────────────────────────────────────────────────────────────────────

log "开始全新安装流程..."

install_packages
enable_bbr
start_service lxcfs

# ZRAM swap
cat > /etc/systemd/zram-generator.conf <<'EOF'
[zram0]
compression-algorithm=lz4
zram-size=ram/2
fs-type=swap
swap-priority=100
EOF
start_service systemd-zram-setup@zram0.service

ARCH=$(detect_arch)
log "架构: $(uname -m) ($ARCH)"

# Podman socket
start_service podman.socket

# XFS 数据盘
if [ -f "/xfs_disk.img" ]; then
    log "数据盘已存在，跳过创建"
    mkdir -p /data
    mountpoint -q /data || mount -o defaults,pquota,loop /xfs_disk.img /data
else
    read -rp "请输入数据盘大小 (例如 20G): " xfs_size
    xfs_size=${xfs_size:-20G}
    create_xfs_disk "/xfs_disk.img" "$xfs_size" "/data" "defaults,pquota,loop"
fi

# Podman 存储配置
cat > /etc/containers/storage.conf <<'EOF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/data/containers/storage"
EOF
mkdir -p /data/containers/storage

# ── IPv6 检测 ────────────────────────────────────────────────────────────────

read -rp "是否进行公网 IPv6 检测? [Y/n]: " _ipv6
_ipv6=${_ipv6:-Y}
IPV6_CONFIG="local"
NDP_EXTRA_FLAGS=""

if [[ "$_ipv6" =~ ^[Yy]$ ]]; then
    IPV6_CONFIG=$(detect_and_configure_ipv6)
    if [ "$IPV6_CONFIG" = "local" ]; then
        read -rp "公网 IPv6 检测失败，是否使用本地网段继续? [Y/n]: " _fb
        [[ "${_fb:-Y}" =~ ^[Nn]$ ]] && { log "已退出"; exit 1; }
    fi
fi

if [ "$IPV6_CONFIG" = "local" ]; then
    # ── 本地 IPv6 网络 ────────────────────────────────────────────────────────
    log "使用本地 IPv6 网段创建 Podman 网络..."
    podman network exists "$PODMAN_NETWORK" 2>/dev/null && podman network rm "$PODMAN_NETWORK"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        --ipv6 \
        --subnet=fd91:cafe:cafe:10::/64 \
        --gateway=fd91:cafe:cafe:10::1 \
        "$PODMAN_NETWORK"
    log "✓ Podman 网络创建完成（本地 IPv6）"

else
    # ── 公网 IPv6 网络 ────────────────────────────────────────────────────────
    IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
    IPV6_ADDR=$(echo  "$IPV6_CONFIG" | cut -d'|' -f2)
    IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)
    log "公网 IPv6: $IPV6_ADDR/$IPV6_PREFIX 网卡: $IPV6_IFACE"

    # 计算容器子网
    NETWORK_PREFIX=$(sipcalc "$IPV6_ADDR/$IPV6_PREFIX" 2>/dev/null | grep "Network range" | awk '{print $4}' | cut -d'-' -f1)
    if [ -z "$NETWORK_PREFIX" ]; then
        EXPANDED=$(expand_ipv6 "$IPV6_ADDR")
    else
        EXPANDED=$(expand_ipv6 "$NETWORK_PREFIX")
    fi
    P1=$(echo "$EXPANDED" | cut -d':' -f1)
    P2=$(echo "$EXPANDED" | cut -d':' -f2)
    P3=$(echo "$EXPANDED" | cut -d':' -f3)
    P4=$(echo "$EXPANDED" | cut -d':' -f4)
    P5=$(echo "$EXPANDED" | cut -d':' -f5)
    P6=$(echo "$EXPANDED" | cut -d':' -f6)

    if [ "$IPV6_PREFIX" -le 64 ]; then
        CONTAINER_BASE="${P1}:${P2}:${P3}:${P4}:cafe:babe:0:0"
        CONTAINER_GW="${P1}:${P2}:${P3}:${P4}:cafe:babe:0:1"
    else
        CONTAINER_BASE="${P1}:${P2}:${P3}:${P4}:${P5}:${P6}:0:0"
        CONTAINER_GW="${P1}:${P2}:${P3}:${P4}:${P5}:${P6}:0:1"
    fi
    log "容器 IPv6 网段: ${CONTAINER_BASE}/112，网关: $CONTAINER_GW"

    # 修改网卡前缀为 /112
    ip addr del "$IPV6_ADDR/$IPV6_PREFIX" dev "$IPV6_IFACE" 2>/dev/null || true
    ip addr add "$IPV6_ADDR/112" dev "$IPV6_IFACE"

    # 持久化到 /etc/network/interfaces
    cp /etc/network/interfaces "/etc/network/interfaces.bak.$(date +%Y%m%d%H%M%S)"
    if grep -q "iface $IPV6_IFACE inet6 static" /etc/network/interfaces; then
        sed -i "/iface $IPV6_IFACE inet6 static/,/^$/s|address.*|address $IPV6_ADDR/112|" /etc/network/interfaces
    else
        cat >> /etc/network/interfaces <<EOF

# IPv6 for containers (runman-agent)
iface $IPV6_IFACE inet6 static
    address $IPV6_ADDR/112
EOF
    fi

    # 创建 Podman 网络
    podman network exists "$PODMAN_NETWORK" 2>/dev/null && podman network rm "$PODMAN_NETWORK"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        --ipv6 \
        --subnet="${CONTAINER_BASE}/112" \
        --gateway="$CONTAINER_GW" \
        "$PODMAN_NETWORK"
    log "✓ Podman 网络创建完成（公网 IPv6）"

    # 等待网络配置文件生成
    for i in $(seq 1 10); do
        [ -f "/etc/containers/networks/${PODMAN_NETWORK}.json" ] && break
        sleep 1
    done

    # 禁用 IPv6 SNAT（netavark）
    NET_JSON="/etc/containers/networks/${PODMAN_NETWORK}.json"
    if [ -f "$NET_JSON" ]; then
        jq '.options = {"snat_ipv4": "true", "snat_ipv6": "false"}' "$NET_JSON" > "${NET_JSON}.tmp" \
            && mv "${NET_JSON}.tmp" "$NET_JSON"
        log "✓ netavark IPv6 SNAT 已禁用"
    fi

    # 下载自定义 netavark
    log "下载自定义 netavark..."
    if download_with_retry \
        "$NETAVARK_BASE/netavark-$ARCH" \
        "/usr/libexec/podman/netavark"; then
        chmod +x /usr/libexec/podman/netavark
        log "✓ netavark 安装完成"
    else
        log "警告: netavark 下载失败，将使用系统默认版本"
    fi

    # NDP flags 将附加到 agent 服务
    NDP_EXTRA_FLAGS=" \\
  --ndp-iface $IPV6_IFACE \\
  --ndp-subnet ${CONTAINER_BASE}/112 \\
  --ndp-network $PODMAN_NETWORK"
    log "✓ NDP responder 将由 runman-agent 内置处理"
fi

# ── Podman 容器自动重启服务 ────────────────────────────────────────────────────

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

# ── 拉取容器镜像 ──────────────────────────────────────────────────────────────

log "拉取容器镜像..."
for image in "docker.io/narwhalcloud/debian:podman" "docker.io/narwhalcloud/alpine:podman"; do
    podman images --format "{{.Repository}}:{{.Tag}}" | grep -q "$image" \
        && log "镜像 $image 已存在" \
        || { log "拉取 $image..."; podman pull "$image" || log "警告: $image 拉取失败"; }
done

# ── 安装 runman-agent 二进制及服务 ────────────────────────────────────────────

download_agent "$ARCH"

# ── 配置 token / server_addr ─────────────────────────────────────────────────

echo ""
read -rp "请输入 Server 地址 (例如 example.com:443): " SERVER_ADDR
read -rp "请输入 Token: " AGENT_TOKEN
echo ""

write_service_file "$SERVER_ADDR" "$AGENT_TOKEN" "$NDP_EXTRA_FLAGS"
start_service "$AGENT_SERVICE"
log "✓ agent 已启动，token/server 通过启动参数写入"

# ── 可选安装 rfw ──────────────────────────────────────────────────────────────

read -rp "是否安装 rfw 防火墙? [Y/n]: " _rfw
_rfw=${_rfw:-Y}

if [[ "$_rfw" =~ ^[Yy]$ ]]; then
    mkdir -p /root/rfw
    RFW_ARCH=$(uname -m)

    if [ ! -f "/root/rfw/rfw" ]; then
        download_with_retry \
            "$RFW_BASE/rfw-$RFW_ARCH-unknown-linux-musl" \
            "/root/rfw/rfw"
        chmod +x /root/rfw/rfw
    else
        log "rfw 已存在，跳过下载"
    fi

    if [ ! -f "/etc/systemd/system/rfw.service" ]; then
        mapfile -t interfaces < <(ip -o link show | awk -F': ' '{print $2}' | grep -v lo)
        echo "可用网络接口："
        for i in "${!interfaces[@]}"; do echo "$((i+1)). ${interfaces[$i]}"; done
        while true; do
            read -rp "请选择网卡编号 (1-${#interfaces[@]}): " choice
            [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#interfaces[@]}" ] && break
            echo "无效选择，请重新输入"
        done
        SEL_IFACE="${interfaces[$((choice-1))]}"
        log "选择网卡: $SEL_IFACE"

        cat > /etc/systemd/system/rfw.service <<EOF
[Unit]
Description=RFW Firewall
After=network.target

[Service]
Type=simple
User=root
Environment=RUST_LOG=info
ExecStart=/root/rfw/rfw --iface $SEL_IFACE --block-email --block-http --block-socks5 --block-fet-strict --block-wireguard --countries CN
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
    else
        log "rfw 服务已存在，跳过"
    fi
    start_service rfw
else
    log "跳过 rfw 安装"
fi

# ── 完成 ──────────────────────────────────────────────────────────────────────

IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "获取失败")
echo ""
log "========================================"
log "✓ 安装完成！"
log "IP:         $IP"
log "Web 管理:   http://$IP:$AGENT_WEB_PORT"
log "========================================"
