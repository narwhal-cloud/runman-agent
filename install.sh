#!/bin/bash
set -e

# ────────────────────────────────────────────────────────────────────────────────
# NarwhalCloud Agent install / update script
# ────────────────────────────────────────────────────────────────────────────────

AGENT_BINARY="/usr/local/bin/runman-agent"
AGENT_SERVICE="runman-agent"
AGENT_DATA_DIR="/var/lib/runman-agent"
AGENT_DB="$AGENT_DATA_DIR/agent.db"
AGENT_WEB_PORT="8792"
PODMAN_NETWORK="narwhal-net"

DOWNLOAD_BASE="https://github.com/narwhal-cloud/runman-agent/releases/latest/download"
NETAVARK_URL="https://github.com/narwhal-cloud/netavark/releases/latest/download/netavark-test"
RFW_BASE="https://github.com/narwhal-cloud/rfw/releases/latest/download"

# ── Language selection ────────────────────────────────────────────────────────

printf "Select language / 选择语言:\n  1) English (default)\n  2) 中文\n"
read -rp "> " _lang_choice
case "${_lang_choice}" in
    2) LANG_CODE="zh" ;;
    *) LANG_CODE="en" ;;
esac

# t EN ZH — returns the string for current language
t() { [ "$LANG_CODE" = "zh" ] && echo "$2" || echo "$1"; }

# ────────────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────────────

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"; }

cleanup_locks() {
    killall apt apt-get dpkg 2>/dev/null || true
    rm -f /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
    dpkg --configure -a 2>/dev/null || true
}

install_packages() {
    local max=3 attempt=1
    while [ $attempt -le $max ]; do
        log "$(t "Installing packages (attempt $attempt)..." "安装软件包 (第 $attempt 次)...")"
        if apt-get update -qq && apt-get install -y curl xfsprogs lxcfs uuid-runtime systemd-zram-generator jq sipcalc podman; then
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

expand_ipv6() {
    local expanded
    expanded=$(sipcalc "$1" 2>/dev/null | grep "Expanded Address" | awk '{print $4}')
    [ -n "$expanded" ] && echo "$expanded" || echo "$1"
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

# Returns "local" or "IFACE|ADDR|PREFIX"
detect_and_configure_ipv6() {
    log "$(t "Detecting public IPv6..." "开始检测公网 IPv6...")" >&2
    if ! curl -6 -s --max-time 10 ip.sb >/dev/null 2>&1; then
        log "$(t "No public IPv6 detected." "未检测到公网 IPv6。")" >&2
        echo "local"; return 0
    fi
    log "$(t "✓ Public IPv6 available." "✓ 公网 IPv6 可用。")" >&2

    local iface
    iface=$(ip -6 route show default 2>/dev/null | head -1 | awk '{print $5}')
    if [ -z "$iface" ]; then
        log "$(t "No default IPv6 route found." "未找到默认 IPv6 路由。")" >&2
        echo "local"; return 0
    fi
    log "$(t "Default IPv6 interface: $iface" "默认 IPv6 网卡: $iface")" >&2

    local ipv6_full ipv6_addr prefix_len
    ipv6_full=$(ip -6 addr show dev "$iface" 2>/dev/null | grep -v "fe80" | grep "scope global" | head -1 | awk '{print $2}')
    if [ -z "$ipv6_full" ]; then
        log "$(t "No global IPv6 address found." "未找到全局 IPv6 地址。")" >&2
        echo "local"; return 0
    fi

    ipv6_addr=$(echo "$ipv6_full" | cut -d'/' -f1)
    prefix_len=$(echo "$ipv6_full" | cut -d'/' -f2)
    log "$(t "IPv6 address: $ipv6_addr/$prefix_len" "IPv6 地址: $ipv6_addr/$prefix_len")" >&2

    if [ "$prefix_len" -gt 112 ]; then
        log "$(t "Prefix /$prefix_len too small for subnetting." "前缀 /$prefix_len 不足以分配子网。")" >&2
        echo "local"; return 0
    fi

    local test_addr
    test_addr=$(ipv6_plus_one "$ipv6_addr")
    ip addr add "$test_addr/$prefix_len" dev "$iface" 2>/dev/null || { echo "local"; return 0; }
    sleep 3
    local ok=0
    curl -6 --interface "$test_addr" -s --max-time 10 ip.sb >/dev/null 2>&1 && ok=1
    ip addr del "$test_addr/$prefix_len" dev "$iface" 2>/dev/null

    if [ $ok -eq 1 ]; then
        log "$(t "✓ Public IPv6 test passed." "✓ 公网 IPv6 测试成功。")" >&2
        echo "$iface|$ipv6_addr|$prefix_len"
    else
        log "$(t "Public IPv6 test failed, using local subnet." "公网 IPv6 测试失败，使用本地网段。")" >&2
        echo "local"
    fi
}

enable_bbr() {
    log "$(t "Configuring BBR..." "配置 BBR...")"
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
EOF
    sysctl -p /etc/sysctl.d/99-narwhalcloud.conf >/dev/null 2>&1 \
        && log "$(t "✓ BBR configured." "✓ BBR 已配置。")" \
        || log "$(t "BBR will take effect after reboot." "BBR 将在重启后生效。")"
}

create_xfs_disk() {
    local disk=$1 size=$2 mount=$3 opts=$4
    if [ ! -f "$disk" ]; then
        log "$(t "Creating XFS disk $disk ($size)..." "创建 XFS 磁盘 $disk ($size)...")"
        fallocate -l "$size" "$disk"
        mkfs.xfs "$disk"
    else
        log "$(t "$disk already exists, skipping." "$disk 已存在，跳过。")"
    fi
    mkdir -p "$mount"
    mountpoint -q "$mount" || mount -o "$opts" "$disk" "$mount"
    grep -q "$disk" /etc/fstab || echo "$disk $mount xfs $opts 0 0" >> /etc/fstab
}

download_agent() {
    local arch=$1
    log "$(t "Downloading NarwhalCloud Agent ($arch)..." "下载 NarwhalCloud Agent ($arch)...")"
    download_with_retry "$DOWNLOAD_BASE/runman-agent-linux-$arch" "$AGENT_BINARY.new"
    chmod +x "$AGENT_BINARY.new"
    mv "$AGENT_BINARY.new" "$AGENT_BINARY"
    log "$(t "✓ NarwhalCloud Agent installed to $AGENT_BINARY" "✓ NarwhalCloud Agent 已安装到 $AGENT_BINARY")"
}

# write_service_file WEB_USER WEB_PASS [NDP_EXTRA_FLAGS]
write_service_file() {
    local web_user=$1 web_pass=$2 extra_flags="${3:-}"
    mkdir -p "$AGENT_DATA_DIR"
    cat > "/etc/systemd/system/$AGENT_SERVICE.service" <<EOF
[Unit]
Description=NarwhalCloud Agent
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
  --web-user $web_user \\
  --web-pass $web_pass${extra_flags}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# ── Update flow ───────────────────────────────────────────────────────────────

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ]; then
    log "$(t "Existing NarwhalCloud Agent detected, updating..." "检测到已安装的 NarwhalCloud Agent，执行更新流程...")"

    ARCH=$(detect_arch)
    download_agent "$ARCH"

    systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null && systemctl restart "$AGENT_SERVICE"
    log "$(t "✓ NarwhalCloud Agent updated." "✓ NarwhalCloud Agent 已更新。")"

    # Update rfw if installed
    if [ -f "/root/rfw/rfw" ] && [ -f "/etc/systemd/system/rfw.service" ]; then
        log "$(t "rfw detected, updating..." "检测到 rfw，开始更新...")"
        EXISTING_IFACE=$(grep "ExecStart=" /etc/systemd/system/rfw.service | grep -oP '(?<=--iface )[^ ]+' || true)
        if [ -n "$EXISTING_IFACE" ]; then
            RFW_ARCH=$(uname -m)
            if download_with_retry "$RFW_BASE/rfw-$RFW_ARCH-unknown-linux-musl" "/root/rfw/rfw.new"; then
                chmod +x /root/rfw/rfw.new
                systemctl is-active --quiet rfw && systemctl stop rfw
                mv /root/rfw/rfw.new /root/rfw/rfw
                sed -i "s|^ExecStart=.*|ExecStart=/root/rfw/rfw --iface $EXISTING_IFACE --block-email --block-http --block-socks5 --block-fet-strict --block-wireguard --countries CN|" \
                    /etc/systemd/system/rfw.service
                systemctl daemon-reload && systemctl start rfw
                log "$(t "✓ rfw updated." "✓ rfw 已更新。")"
            fi
        fi
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

install_packages
enable_bbr
start_service lxcfs

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

start_service podman.socket

# XFS data disk
if [ -f "/xfs_disk.img" ]; then
    log "$(t "Data disk already exists, skipping." "数据盘已存在，跳过。")"
    mkdir -p /data
    mountpoint -q /data || mount -o defaults,pquota,loop /xfs_disk.img /data
else
    read -rp "$(t "Enter data disk size (e.g. 20G): " "请输入数据盘大小 (例如 20G): ")" xfs_size
    xfs_size=${xfs_size:-20G}
    create_xfs_disk "/xfs_disk.img" "$xfs_size" "/data" "defaults,pquota,loop"
fi

cat > /etc/containers/storage.conf <<'EOF'
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "/data/containers/storage"
EOF
mkdir -p /data/containers/storage

# ── IPv6 detection ────────────────────────────────────────────────────────────

read -rp "$(t "Enable public IPv6 detection? [Y/n]: " "是否进行公网 IPv6 检测? [Y/n]: ")" _ipv6
IPV6_CONFIG="local"
NDP_EXTRA_FLAGS=""

if [[ "${_ipv6:-Y}" =~ ^[Yy]$ ]]; then
    IPV6_CONFIG=$(detect_and_configure_ipv6)
    if [ "$IPV6_CONFIG" = "local" ]; then
        read -rp "$(t "IPv6 detection failed. Continue with local subnet? [Y/n]: " "公网 IPv6 检测失败，是否使用本地网段继续? [Y/n]: ")" _fb
        [[ "${_fb:-Y}" =~ ^[Nn]$ ]] && { log "$(t "Aborted." "已退出。")"; exit 1; }
    fi
fi

if podman network exists "$PODMAN_NETWORK" 2>/dev/null; then
    log "$(t "Podman network $PODMAN_NETWORK already exists, skipping." "Podman 网络 $PODMAN_NETWORK 已存在，跳过创建。")"
elif [ "$IPV6_CONFIG" = "local" ]; then
    log "$(t "Creating Podman network with local IPv6..." "使用本地 IPv6 网段创建 Podman 网络...")"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        --ipv6 \
        --subnet=fd91:cafe:cafe:10::/64 \
        --gateway=fd91:cafe:cafe:10::1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (local IPv6)." "✓ Podman 网络创建完成（本地 IPv6）。")"

else
    IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
    IPV6_ADDR=$(echo  "$IPV6_CONFIG" | cut -d'|' -f2)
    IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)
    log "$(t "Public IPv6: $IPV6_ADDR/$IPV6_PREFIX on $IPV6_IFACE" "公网 IPv6: $IPV6_ADDR/$IPV6_PREFIX 网卡: $IPV6_IFACE")"

    # 用 python3 计算容器子网（Debian 13 标配 python3）
    # 策略：取宿主机前缀，在其内偏移固定量得到 /112 容器段，避免与 EUI-64/隐私地址冲突
    read -r CONTAINER_BASE CONTAINER_GW <<< "$(python3 - <<PYEOF
import ipaddress
net = ipaddress.IPv6Network('$IPV6_ADDR/$IPV6_PREFIX', strict=False)
base_int = int(net.network_address)
prefix = int('$IPV6_PREFIX')
# 在前缀内取偏移量 0xcafe（51966），放在 /112 边界上
offset = 0xcafe << (128 - prefix - 16) if prefix <= 112 else 0
container_int = (base_int | offset) & ~((1 << (128 - 112)) - 1)
container_net = ipaddress.IPv6Network(f'{ipaddress.IPv6Address(container_int)}/112', strict=False)
gw = container_net.network_address + 1
print(str(container_net.network_address), str(gw))
PYEOF
)"
    log "$(t "Container subnet: ${CONTAINER_BASE}/112  gateway: ${CONTAINER_GW}" "容器子网: ${CONTAINER_BASE}/112  网关: ${CONTAINER_GW}")"

    # 先禁止 SLAAC 自动配置，防止后续再生成 /64 地址（保留 RA 默认路由）
    sysctl -w "net.ipv6.conf.$IPV6_IFACE.autoconf=0" > /dev/null
    echo "net.ipv6.conf.$IPV6_IFACE.autoconf=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    # 删除该网卡上所有 global scope 的 /64 地址（含隐私地址 mngtmpaddr），
    # 消除对应的 /64 内核连接路由——netavark 检测到该路由时会拒绝创建子集子网。
    while IFS= read -r _addr64; do
        ip addr del "$_addr64" dev "$IPV6_IFACE" 2>/dev/null || true
    done < <(ip -6 addr show dev "$IPV6_IFACE" scope global | awk '/inet6.*\/64/{print $2}')
    # 以 /128 重新添加宿主机公网地址，不产生连接路由
    ip addr add "$IPV6_ADDR/128" dev "$IPV6_IFACE" 2>/dev/null || true
    log "$(t "✓ All /64 addresses removed, host IPv6 re-added as /128, SLAAC disabled." "✓ 已删除所有 /64 地址，宿主机 IPv6 已改为 /128，SLAAC 已禁用。")"

    # 先安装自定义 netavark（支持 snat_ipv6 选项），再操作网络
    if download_with_retry "$NETAVARK_URL" "/usr/libexec/podman/netavark"; then
        chmod +x /usr/libexec/podman/netavark
        log "$(t "✓ Custom netavark installed." "✓ 自定义 netavark 已安装。")"
    else
        log "$(t "Warning: netavark download failed, using system default." "警告: netavark 下载失败，使用系统默认版本。")"
    fi

    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 --gateway=10.91.0.1 \
        --ipv6 \
        --subnet="${CONTAINER_BASE}/112" --gateway="$CONTAINER_GW" \
        "$PODMAN_NETWORK"
    # snat_ipv6=false 不能通过 CLI 传入（Podman CLI 有白名单校验），直接注入 JSON。
    NETWORK_JSON="/etc/containers/networks/${PODMAN_NETWORK}.json"
    jq '.options["snat_ipv6"] = "false"' "$NETWORK_JSON" > "${NETWORK_JSON}.tmp" \
        && mv "${NETWORK_JSON}.tmp" "$NETWORK_JSON" \
        && log "$(t "✓ snat_ipv6=false injected into network config." "✓ snat_ipv6=false 已注入网络配置。")" \
        || log "$(t "Warning: failed to inject snat_ipv6=false." "警告: 注入 snat_ipv6=false 失败。")"

    NDP_EXTRA_FLAGS=" \\
  --ndp-iface $IPV6_IFACE \\
  --ndp-subnet ${CONTAINER_BASE}/112 \\
  --ndp-network $PODMAN_NETWORK"
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

# ── Web panel credentials ─────────────────────────────────────────────────────

echo ""
log "$(t "Set up NarwhalCloud panel credentials (protects port $AGENT_WEB_PORT)" "设置 NarwhalCloud 面板访问凭据（保护端口 $AGENT_WEB_PORT）")"
read -rp "$(t "Username [admin]: " "用户名 [admin]: ")" WEB_USER
WEB_USER=${WEB_USER:-admin}
while true; do
    read -rsp "$(t "Password: " "密码: ")" WEB_PASS; echo
    read -rsp "$(t "Confirm password: " "确认密码: ")" WEB_PASS2; echo
    [ "$WEB_PASS" = "$WEB_PASS2" ] && break
    log "$(t "Passwords do not match, please try again." "两次输入不一致，请重新输入。")"
done
echo ""

# ── Install agent ─────────────────────────────────────────────────────────────

download_agent "$ARCH"
write_service_file "$WEB_USER" "$WEB_PASS" "$NDP_EXTRA_FLAGS"
start_service "$AGENT_SERVICE"

# ── Optional rfw ─────────────────────────────────────────────────────────────

read -rp "$(t "Install rfw firewall? [Y/n]: " "是否安装 rfw 防火墙? [Y/n]: ")" _rfw
if [[ "${_rfw:-Y}" =~ ^[Yy]$ ]]; then
    mkdir -p /root/rfw
    RFW_ARCH=$(uname -m)
    if [ ! -f "/root/rfw/rfw" ]; then
        download_with_retry "$RFW_BASE/rfw-$RFW_ARCH-unknown-linux-musl" "/root/rfw/rfw"
        chmod +x /root/rfw/rfw
    else
        log "$(t "rfw already exists, skipping download." "rfw 已存在，跳过下载。")"
    fi

    if [ ! -f "/etc/systemd/system/rfw.service" ]; then
        mapfile -t interfaces < <(ip -o link show | awk -F': ' '{print $2}' | grep -v lo)
        echo "$(t "Available interfaces:" "可用网络接口：")"
        for i in "${!interfaces[@]}"; do echo "  $((i+1)). ${interfaces[$i]}"; done
        while true; do
            read -rp "$(t "Select interface number (1-${#interfaces[@]}): " "请选择网卡编号 (1-${#interfaces[@]}): ")" choice
            [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#interfaces[@]}" ] && break
            echo "$(t "Invalid choice, please try again." "无效选择，请重新输入。")"
        done
        SEL_IFACE="${interfaces[$((choice-1))]}"
        log "$(t "Selected interface: $SEL_IFACE" "选择网卡: $SEL_IFACE")"

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
        log "$(t "rfw service already exists, skipping." "rfw 服务已存在，跳过。")"
    fi
    start_service rfw
else
    log "$(t "Skipping rfw installation." "跳过 rfw 安装。")"
fi

# ── Done ──────────────────────────────────────────────────────────────────────

IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
echo ""
log "========================================"
log "$(t "✓ NarwhalCloud Agent installation complete!" "✓ NarwhalCloud Agent 安装完成！")"
log "$(t "IP:           $IP" "IP:           $IP")"
log "$(t "Web panel:    http://$IP:$AGENT_WEB_PORT" "面板地址:     http://$IP:$AGENT_WEB_PORT")"
log "$(t "Username:     $WEB_USER" "用户名:       $WEB_USER")"
echo ""
log "$(t "Next step: log in to the web panel and enter your Token" "下一步：登录面板并在设置中填入您的 Token")"
log "========================================"
