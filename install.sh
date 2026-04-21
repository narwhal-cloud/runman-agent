#!/bin/bash
set -e

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
NETAVARK_BASE="https://github.com/narwhal-cloud/netavark/releases/latest/download"
# rfw v2 随 agent 同仓库发布，资产名为 rfw-amd64 / rfw-arm64
RFW_BASE="$DOWNLOAD_BASE"

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
    local virt_type="${1:-podman}"
    local max=3 attempt=1
    local base_packages="curl wget python3 uuid-runtime systemd-zram-generator jq chrony iptables iproute2 fdisk bc dnsutils"
    local extra_packages

    if [ "$virt_type" = "cloudhv" ]; then
        extra_packages="qemu-utils cloud-image-utils"
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
    if ! curl -6 -s --max-time 10 ip.sb >/dev/null 2>&1; then
        log "$(t "No public IPv6 detected." "未检测到公网 IPv6。")" >&2
        echo "none"; return 0
    fi
    log "$(t "✓ Public IPv6 available." "✓ 公网 IPv6 可用。")" >&2

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

    # 对于 /64 或更大的子网，直接接受（无需测试新地址，因为 BGP 学习延迟可能导致测试失败）
    # 对于 /128 单地址，直接接受（SNAT 模式）
    # 只对 /65-/127 的小子网进行可达性测试
    if [ "$prefix_len" -le 64 ] || [ "$prefix_len" -eq 128 ]; then
        log "$(t "✓ Public IPv6 address detected (/$prefix_len)." "✓ 检测到公网 IPv6 地址 (/$prefix_len)。")" >&2
        echo "$iface|$ipv6_addr|$prefix_len"
    elif [ "$prefix_len" -gt 64 ] && [ "$prefix_len" -lt 128 ]; then
        # 测试 /65-/127 的小子网
        local test_addr
        test_addr=$(ipv6_plus_one "$ipv6_addr")
        ip addr add "$test_addr/$prefix_len" dev "$iface" 2>/dev/null || {
            log "$(t "Cannot add test address, falling back to SNAT." "无法添加测试地址，回退到 SNAT 模式。")" >&2
            echo "$iface|$ipv6_addr|128"; return 0
        }
        sleep 3
        local ok=0
        curl -6 --interface "$test_addr" -s --max-time 10 ip.sb >/dev/null 2>&1 && ok=1
        ip addr del "$test_addr/$prefix_len" dev "$iface" 2>/dev/null

        if [ $ok -eq 1 ]; then
            log "$(t "✓ Public IPv6 test passed." "✓ 公网 IPv6 测试成功。")" >&2
            echo "$iface|$ipv6_addr|$prefix_len"
        else
            # 子网不可用，但宿主机 IPv6 连通（curl -6 已在函数开头通过），回退到 SNAT
            log "$(t "Subnet not usable, falling back to SNAT via host address." "子网不可用，回退到宿主机地址 SNAT 模式。")" >&2
            echo "$iface|$ipv6_addr|128"
        fi
    else
        echo "none"; return 0
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

    mkdir -p "$AGENT_CONFIG_DIR"

    cat > "$AGENT_CONFIG_FILE" <<EOF
{
    "db": "$AGENT_DB",
    "web": ":$AGENT_WEB_PORT",
    "virt_type": "$virt_type",
    "token": "",
    "monitor_nic": "",
    "monitor_disk": "/",
    "web_user": "",
    "web_pass_hash": "",
    "host": "",
    "max_port_forward": 20,
    "ipv6_mode": "$ipv6_mode",
    "ipv6_subnet": "$ipv6_subnet",
    "ipv6_addr": "$ipv6_addr",
    "ipv6_iface": "$ipv6_iface",
    "ndp_iface": "$ndp_iface",
    "ndp_subnets": "$ndp_subnets",
    "ndp_network": "$ndp_network",
    "rfw_addr": "$RFW_API_ADDR"
}
EOF
    chmod 600 "$AGENT_CONFIG_FILE"
    log "$(t "✓ Configuration file written to $AGENT_CONFIG_FILE" "✓ 配置文件已写入 $AGENT_CONFIG_FILE")"
}

# ── Update flow ───────────────────────────────────────────────────────────────

if systemctl is-active --quiet "$AGENT_SERVICE" 2>/dev/null || [ -f "$AGENT_BINARY" ]; then
    log "$(t "Existing NarwhalCloud Agent detected, updating..." "检测到已安装的 NarwhalCloud Agent，执行更新流程...")"

    ARCH=$(detect_arch)
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

    # Update rfw if binary exists
    if [ -f "$RFW_BIN_DIR/rfw" ]; then
        log "$(t "rfw detected, updating..." "检测到 rfw，开始更新...")"
        if download_with_retry "$RFW_BASE/rfw-$ARCH" "$RFW_BIN_DIR/rfw.new"; then
            chmod +x "$RFW_BIN_DIR/rfw.new"
            systemctl is-active --quiet rfw && systemctl stop rfw
            mv "$RFW_BIN_DIR/rfw.new" "$RFW_BIN_DIR/rfw"
            systemctl is-active --quiet rfw || systemctl is-enabled --quiet rfw 2>/dev/null \
                && { systemctl daemon-reload; systemctl start rfw; }
            log "$(t "✓ rfw updated." "✓ rfw 已更新。")"
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


# ── VM Images Updater ────────────────────────────────────────────────────────

update_vm_images() {
    local IMGDIR="${VM_IMAGE_DIR:-/opt/vm-images}"
    local FORCE=0
    local TARGETS=(debian alpine)

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

    customize_debian() {
        local root="$1"
        # Debian cloud image has no SSH password restrictions — nothing needed by default
        : # no-op, placeholder for future customizations
    }

    customize_alpine() {
        local root="$1"
        # Alpine nocloud cloud image ships with cloud-init — nothing needed by default
        : # no-op, placeholder for future customizations
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

        local cloud_base="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/cloud"

        # Resolve latest nocloud bios+cloudinit qcow2
        local latest_qcow2
        latest_qcow2=$(wget -qO- "$cloud_base/" \
            | grep -o 'nocloud_alpine-[0-9][^"]*-x86_64-bios-cloudinit-r[0-9]*\.qcow2' \
            | sort -uV | tail -1)
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
enable_bbr
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
    
    # 先禁止 SLAAC 自动配置，防止后续再生成 /64 地址（保留 RA 默认路由）
    sysctl -w "net.ipv6.conf.$iface.autoconf=0" > /dev/null
    echo "net.ipv6.conf.$iface.autoconf=0" >> /etc/sysctl.d/99-narwhalcloud.conf
    
    # 获取当前的默认 IPv6 网关
    local _gw6
    _gw6=$(ip -6 route show default dev "$iface" | awk '{print $3}')

    # 删除该网卡上所有 global scope 的 /64 地址（含隐私地址 mngtmpaddr），消除对应的 /64 内核连接路由。
    while IFS= read -r _addr64; do
        ip addr del "$_addr64" dev "$iface" 2>/dev/null || true
    done < <(ip -6 addr show dev "$iface" scope global | awk '/inet6.*\/64/{print $2}')
    
    # 以 /128 重新添加宿主机公网地址，不产生连接路由
    ip addr add "$addr/128" dev "$iface" 2>/dev/null || true
    
    # 恢复默认路由
    if [ -n "$_gw6" ]; then
        ip -6 route add default via "$_gw6" dev "$iface" 2>/dev/null || true
    fi
    
    log "$(t "✓ All /64 addresses removed, host IPv6 re-added as /128, SLAAC disabled." "✓ 已删除所有 /64 地址，宿主机 IPv6 已改为 /128，SLAAC 已禁用。")"
}

# ── IPv6 detection ────────────────────────────────────────────────────────────

# 初始化 IPv6 模式变量
# IPV6_CONFIG: 检测原始结果，"none" 或 "iface|addr|prefix"
# IPV6_MODE:   最终运行模式，"none" / "snat" / "subnet"（写入 config.json）
# 保存用户通过环境变量显式指定的值（如 IPV6_MODE=subnet bash install.sh）
_USER_IPV6_MODE="${IPV6_MODE:-}"
IPV6_MODE="none"

read -rp "$(t "Enable public IPv6 detection? [Y/n]: " "是否进行公网 IPv6 检测? [Y/n]: ")" _ipv6
IPV6_CONFIG="none"

if [[ "${_ipv6:-Y}" =~ ^[Yy]$ ]]; then
    IPV6_CONFIG=$(detect_and_configure_ipv6)
    if [ "$IPV6_CONFIG" = "none" ]; then
        read -rp "$(t "No IPv6 connectivity detected. Continue without IPv6? [Y/n]: " "未检测到 IPv6 连通性，是否不配置 IPv6 继续? [Y/n]: ")" _fb
        [[ "${_fb:-Y}" =~ ^[Nn]$ ]] && { log "$(t "Aborted." "已退出。")"; exit 1; }
    fi
fi

# ── Network setup (conditional on virtualization type) ────────────────────────

if [ "$VIRT_TYPE" = "cloudhv" ] || [ "$VIRT_TYPE" = "incus" ]; then
    # Cloud-hypervisor / Incus: IPv6 detection and configuration
    log "$(t "Setting up $VIRT_TYPE network..." "配置 $VIRT_TYPE 网络...")"
    
    if [ "$VIRT_TYPE" = "cloudhv" ]; then
        mkdir -p /opt/vm-images/instances /run/cloud-hypervisor
    fi

    # 检测 IPv6 可分配情况（支持任意前缀）
    # 如果用户通过环境变量指定了 IPv6_MODE，则直接使用；否则自动检测
    IPV6_SUBNET=""
    IPV6_ADDR=""
    IPV6_IFACE=""

    # 无公网 IPv6 时保持 IPV6_MODE="none"，不配置 IPv6
    if [ "$IPV6_CONFIG" = "none" ]; then
        log "$(t "No public IPv6, IPv6 will not be configured." "无公网 IPv6，将不配置 IPv6。")"
    fi

    # 尝试获取可分配的 IPv6 子网或地址
    if [ "$IPV6_CONFIG" != "none" ]; then
        IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
        IPV6_ADDR=$(echo "$IPV6_CONFIG" | cut -d'|' -f2)
        IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)

        # 支持的前缀范围：/127 或更小（即使是 /64 也支持）
        # 分配策略：
        #   - 用户指定 IPV6_MODE=snat：SNAT 模式（VM 用私有 ULA，宿主机做 MASQUERADE）
        #   - 用户指定 IPV6_MODE=subnet：subnet 模式（VM 各自获得独立地址）
        #   - 自动检测：
        #     - /128：使用 SNAT 模式
        #     - <= 127：使用 subnet 模式
        if [ -n "${_USER_IPV6_MODE}" ]; then
            # 用户通过环境变量显式指定了模式
            IPV6_MODE="$_USER_IPV6_MODE"
            log "$(t "✓ IPv6 mode explicitly set to: $IPV6_MODE" "✓ IPv6 模式已指定为: $IPV6_MODE")"
        elif [ "$IPV6_PREFIX" = "128" ]; then
            IPV6_MODE="snat"
            log "$(t "✓ IPv6 single address detected (/$IPV6_PREFIX): $IPV6_ADDR, using SNAT mode." "✓ 检测到 IPv6 单地址 (/$IPV6_PREFIX): $IPV6_ADDR，使用 SNAT 模式。")"
        elif [ "$IPV6_PREFIX" -le 127 ]; then
            IPV6_MODE="subnet"
            # 计算可分配子网：使用宿主机地址作为基准
            # 例如：如果宿主机是 2001:db8::/64，则 VM 从 2001:db8::/64 中的 ::2, ::3... 分配
            # 如果宿主机是 2001:db8:1:2::/127，则 VM 用对端地址

            # Python3 计算可分配子网（Debian 13 标配）
            read -r IPV6_SUBNET <<< "$(python3 - <<PYEOF
import ipaddress
host_prefix = int('$IPV6_PREFIX')
# 可分配的最小单位是 /128（单个地址）
# 为了支持多 VM，我们分配 /127 或 /128 级别的地址
# 简化：直接用主地址段作为子网前缀
addr = ipaddress.IPv6Address('$IPV6_ADDR')
net = ipaddress.IPv6Network(f'{addr}/{host_prefix}', strict=False)
# 返回网络地址（用作 VMs 分配的基准）
print(str(net))
PYEOF
)"
            log "$(t "✓ IPv6 subnet detected (/$IPV6_PREFIX): $IPV6_SUBNET, VMs will get independent addresses." "✓ 检测到 IPv6 子网 (/$IPV6_PREFIX): $IPV6_SUBNET，VM 将获得独立地址。")"
        else
            # 前缀 > 127，不支持（理论不存在）
            log "$(t "✗ Invalid IPv6 prefix: /$IPV6_PREFIX (must be <= 127)" "✗ 无效的 IPv6 前缀: /$IPV6_PREFIX（必须 <= 127）")"
            IPV6_MODE="none"
        fi
        
        if [ "$IPV6_MODE" = "subnet" ]; then
            configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"
        fi
    fi

    # Bridge creation will be handled by the agent's ensureBridge() on startup
    log "$(t "✓ $VIRT_TYPE directories and basic network logic configured." "✓ $VIRT_TYPE 目录及基础网络逻辑配置完成。")"

    # NDP responder 配置保存到 config.json，将在后面统一生成
    if [ "$IPV6_MODE" = "subnet" ] && [ -n "$IPV6_SUBNET" ] && [ -n "$IPV6_IFACE" ]; then
        log "$(t "✓ NDP responder will be enabled for IPv6 subnet mode." "✓ NDP 响应器将在 IPv6 子网模式下启用。")"
    fi
elif [ "$VIRT_TYPE" = "podman" ]; then
    # Podman: create Podman network

if podman network exists "$PODMAN_NETWORK" 2>/dev/null; then
    log "$(t "Podman network $PODMAN_NETWORK already exists, skipping." "Podman 网络 $PODMAN_NETWORK 已存在，跳过创建。")"
elif [ "$IPV6_CONFIG" = "none" ]; then
    # IPV6_MODE 保持 "none"，不配置 IPv6
    log "$(t "Creating Podman network (IPv4 only)..." "创建 Podman 网络（仅 IPv4）...")"
    podman network create \
        --driver=bridge \
        --subnet=10.91.0.0/20 \
        --gateway=10.91.0.1 \
        "$PODMAN_NETWORK"
    log "$(t "✓ Podman network created (IPv4 only, no IPv6)." "✓ Podman 网络创建完成（仅 IPv4，无 IPv6）。")"

else
    IPV6_IFACE=$(echo "$IPV6_CONFIG" | cut -d'|' -f1)
    IPV6_ADDR=$(echo  "$IPV6_CONFIG" | cut -d'|' -f2)
    IPV6_PREFIX=$(echo "$IPV6_CONFIG" | cut -d'|' -f3)
    log "$(t "Public IPv6: $IPV6_ADDR/$IPV6_PREFIX on $IPV6_IFACE" "公网 IPv6: $IPV6_ADDR/$IPV6_PREFIX 网卡: $IPV6_IFACE")"

    if [ "$IPV6_PREFIX" = "128" ]; then
        # /128 是单个地址，无法划分子网给容器，回退到 ULA + SNAT 模式
        IPV6_MODE="snat"
        log "$(t "Public IPv6 is /128 (single address): cannot allocate public subnet, using ULA with SNAT." "公网 IPv6 为 /128（单个地址）：无法分配公网子网，使用 ULA + SNAT 模式。")"
        podman network create \
            --driver=bridge \
            --subnet=10.91.0.0/20 \
            --gateway=10.91.0.1 \
            --ipv6 \
            --subnet=fd91:cafe:cafe:10::/64 \
            --gateway=fd91:cafe:cafe:10::1 \
            "$PODMAN_NETWORK"
        log "$(t "✓ Podman network created (ULA IPv6, SNAT via host /128)." "✓ Podman 网络创建完成（ULA IPv6，通过宿主机 /128 SNAT）。")"
        # /128 SNAT 模式：不设 NDP，不注入 snat_ipv6=false
    else
        # 有公网前缀（<= /127），用 python3 计算容器子网
        IPV6_MODE="subnet"
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

        configure_host_ipv6_routing "$IPV6_IFACE" "$IPV6_ADDR"

        # 先安装自定义 netavark（支持 snat_ipv6 选项），再操作网络
        if download_with_retry "$NETAVARK_BASE/netavark-new-$ARCH" "/usr/libexec/podman/netavark"; then
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

        # NDP responder will be handled by NarwhalCloud Agent
        NDP_IFACE="$IPV6_IFACE"
        NDP_SUBNETS="${CONTAINER_BASE}/112"
        NDP_NETWORK="$PODMAN_NETWORK"
        log "$(t "✓ NDP responder will be handled by NarwhalCloud Agent." "✓ NDP responder 将由 NarwhalCloud Agent 内置处理。")"
    fi
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

    # Download base VM images (Debian, Alpine)
    update_vm_images
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

# 生成配置文件（包含所有启动参数、IPv6配置和磁盘限制）
write_config_file "$VIRT_TYPE" "$IPV6_MODE" "$IPV6_SUBNET" "$IPV6_ADDR" "$IPV6_IFACE" "$NDP_IFACE" "$NDP_SUBNETS" "$NDP_NETWORK"

write_service_file "$VIRT_TYPE"
start_service "$AGENT_SERVICE"

# ── Optional rfw ─────────────────────────────────────────────────────────────

read -rp "$(t "Install rfw firewall? [Y/n]: " "是否安装 rfw 防火墙? [Y/n]: ")" _rfw
if [[ "${_rfw:-Y}" =~ ^[Yy]$ ]]; then
    mkdir -p "$RFW_BIN_DIR"
    RFW_ARCH=$(detect_arch)

    # 调试模式：检查本地文件
    if [ -f "./rfw-$RFW_ARCH" ]; then
        log "$(t "Found local rfw binary: ./rfw-$RFW_ARCH (debug mode)" "找到本地 rfw 二进制: ./rfw-$RFW_ARCH (调试模式)")"
        cp "./rfw-$RFW_ARCH" "$RFW_BIN_DIR/rfw"
        chmod +x "$RFW_BIN_DIR/rfw"
    elif [ ! -f "$RFW_BIN_DIR/rfw" ]; then
        download_with_retry "$RFW_BASE/rfw-$RFW_ARCH" "$RFW_BIN_DIR/rfw"
        chmod +x "$RFW_BIN_DIR/rfw"
    else
        log "$(t "rfw already exists, skipping download." "rfw 已存在，跳过下载。")"
    fi

    if [ ! -f "/etc/systemd/system/rfw.service" ]; then
        # 过滤掉 lo 及虚拟网桥
        mapfile -t interfaces < <(ip -o link show | awk -F': ' '{print $2}' \
            | grep -v "^lo$" | grep -v "^narwhal-net$" | grep -v "^incusbr0$" \
            | grep -v "^podman" | grep -v "@")

        if [ ${#interfaces[@]} -eq 0 ]; then
            log "$(t "Error: no available WAN interfaces found." "错误: 未找到可用的 WAN 网卡。")"
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
ExecStart=$RFW_BIN_DIR/rfw --iface $SEL_IFACE --api-addr $RFW_API_ADDR
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        log "$(t "✓ rfw service file written. API listens on $RFW_API_ADDR (local only)." "✓ rfw 服务文件已写入，API 监听 $RFW_API_ADDR（仅本地）。")"
    else
        log "$(t "rfw service already exists, skipping." "rfw 服务已存在，跳过。")"
    fi
    start_service rfw
    log "$(t "✓ rfw firewall installed. Manage rules via the agent web panel → Firewall tab." "✓ rfw 防火墙已安装。通过 agent 面板 → Firewall 标签管理规则。")"
else
    log "$(t "Skipping rfw installation." "跳过 rfw 安装。")"
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
fi

IP=$(curl -4 -s --max-time 10 ip.sb 2>/dev/null || echo "N/A")
echo ""
log "========================================"
log "$(t "✓ NarwhalCloud Agent installation complete!" "✓ NarwhalCloud Agent 安装完成！")"
log "$(t "IP:           $IP" "IP:           $IP")"
log "$(t "Web panel:    http://$IP:$AGENT_WEB_PORT" "面板地址:     http://$IP:$AGENT_WEB_PORT")"
echo ""
log "$(t "Next step: log in to the web panel and enter your Token" "下一步：登录面板并在设置中填入您的 Token")"
log "========================================"
