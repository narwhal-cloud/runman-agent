#!/usr/bin/env bash
# vm-instance.sh - Manage cloud-hypervisor VM instances
# Supported distros: debian, alpine
#
# Commands:
#   create <name> <distro> [--system-size 20G] [--data-size 50G] [--cpus 2] [--memory 1024M] [--bandwidth 100]
#   start  <name> [--cpus 2] [--memory 1024M] [--bandwidth 100]
#   stop   <name>
#   status
#   list
#   delete <name>
#   resize-system <name> <new-size>   e.g. 30G
#   info   <name>

set -euo pipefail

IMGDIR="${VM_IMAGE_DIR:-/opt/vm-images}"
INSTDIR="$IMGDIR/instances"
RUNDIR="/run/cloud-hypervisor"
SCRIPTDIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="${CH_BINARY:-$SCRIPTDIR/cloud-hypervisor-static}"

GW_IP="192.168.249.1"
NET_PREFIX="192.168.249"
TAP_BASE="vmtap"

mkdir -p "$INSTDIR" "$RUNDIR"

die() { echo "ERROR: $*" >&2; exit 1; }
log() { echo "[$(date '+%H:%M:%S')] $*"; }

# Generate a per-instance cloud-init ISO (user: root / admin@123)
# Alpine uses cloud-init v1 network config (ifupdown/ENI renderer, no netplan)
# Debian uses cloud-init v2 network config (netplan)
gen_cloudinit() {
    local dir="$1" name="$2" ip="$3" mac="$4" distro="$5"
    local passwd
    passwd=$(openssl passwd -6 -salt saltsalt 'admin@123')

    mkdir -p "$dir/cloudinit"

    cat > "$dir/cloudinit/user-data" <<EOF
#cloud-config
chpasswd:
  expire: false
  users:
    - name: root
      password: $passwd
      type: hash
ssh_pwauth: true
disable_root: false
write_files:
  - path: /etc/resolv.conf
    content: |
      nameserver 1.1.1.1
    permissions: '0644'
runcmd:
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - systemctl restart sshd 2>/dev/null || service sshd restart 2>/dev/null || true
EOF

    cat > "$dir/cloudinit/meta-data" <<EOF
instance-id: $name
local-hostname: $name
EOF

    if [ "$distro" = "alpine" ]; then
        # v1 format: Alpine cloud-init uses ifupdown (ENI) renderer
        cat > "$dir/cloudinit/network-config" <<EOF
version: 1
config:
  - type: physical
    name: eth0
    mac_address: "$mac"
    subnets:
      - type: static
        address: $ip/24
        gateway: $GW_IP
        dns_nameservers:
          - 1.1.1.1
EOF
    else
        # v2 format: Debian supports netplan/v2
        cat > "$dir/cloudinit/network-config" <<EOF
version: 2
ethernets:
  eth0:
    match:
      macaddress: "$mac"
    addresses: [$ip/24]
    gateway4: $GW_IP
    nameservers:
      addresses: [1.1.1.1]
EOF
    fi

    local img="$dir/cloudinit.img"
    rm -f "$img"
    if command -v cloud-localds &>/dev/null; then
        cloud-localds --network-config="$dir/cloudinit/network-config" \
            "$img" "$dir/cloudinit/user-data" "$dir/cloudinit/meta-data"
    else
        # Fallback: mkdosfs + mcopy
        mkdosfs -n CIDATA -C "$img" 8192 -q
        mcopy -oi "$img" "$dir/cloudinit/user-data" ::user-data
        mcopy -oi "$img" "$dir/cloudinit/meta-data" ::meta-data
        mcopy -oi "$img" "$dir/cloudinit/network-config" ::network-config
    fi
    log "  cloud-init: user=root pass=admin@123"
}

# ── Network helpers ───────────────────────────────────────────────────────────

# Assign a unique IP and TAP per instance based on a simple index file
alloc_network() {
    local name="$1"
    local cfg="$INSTDIR/$name/network"

    if [ -f "$cfg" ]; then
        # shellcheck source=/dev/null
        source "$cfg"; return
    fi

    # Find next available index (2-254, 1 is gateway)
    local idx=2
    while grep -rq "IDX=$idx$" "$INSTDIR"/*/network 2>/dev/null; do
        ((idx++))
    done
    [ "$idx" -lt 255 ] || die "No free IPs in $NET_PREFIX.0/24"

    TAP="${TAP_BASE}${idx}"
    MAC=$(printf "52:54:00:00:00:%02x" "$idx")
    IP="${NET_PREFIX}.${idx}"

    cat > "$cfg" <<EOF
IDX=$idx
TAP=$TAP
MAC=$MAC
IP=$IP
EOF
    source "$cfg"
}

BRIDGE="br-vms"

setup_bridge() {
    if ! ip link show "$BRIDGE" &>/dev/null; then
        ip link add "$BRIDGE" type bridge
        ip addr add "${GW_IP}/24" dev "$BRIDGE"
        ip link set "$BRIDGE" up
    fi
    sysctl -q -w net.ipv4.ip_forward=1
    local outif
    outif=$(ip route | awk '/^default/{print $5; exit}')
    iptables -t nat -C POSTROUTING -s "${NET_PREFIX}.0/24" -o "$outif" -j MASQUERADE 2>/dev/null \
        || iptables -t nat -A POSTROUTING -s "${NET_PREFIX}.0/24" -o "$outif" -j MASQUERADE
}

setup_tap() {
    setup_bridge
    if ! ip link show "$TAP" &>/dev/null; then
        ip tuntap add "$TAP" mode tap
    fi
    ip link set "$TAP" master "$BRIDGE"
    ip link set "$TAP" up
}

# Build net rate-limit suffix: "bw_size=N,bw_one_time_burst=N,bw_refill_time=1000"
# $1 = bandwidth in Mbps (0 or empty = no limit)
net_rate_args() {
    local mbps="${1:-0}"
    [ "$mbps" -gt 0 ] 2>/dev/null || return 0
    local bps=$(( mbps * 1000000 / 8 ))
    echo "bw_size=${bps},bw_one_time_burst=${bps},bw_refill_time=1000"
}

# ── Commands ──────────────────────────────────────────────────────────────────

cmd_create() {
    local name="${1:-}" distro="${2:-}"
    [ -n "$name" ]   || die "Usage: $0 create <name> <distro> [options]"
    [ -n "$distro" ] || die "Usage: $0 create <name> <distro> [options]"

    local system_size="20G" data_size="" cpus="2" memory="1024M" bandwidth="0"
    shift 2
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --system-size) system_size="$2"; shift 2 ;;
            --data-size)   data_size="$2";   shift 2 ;;
            --cpus)        cpus="$2";        shift 2 ;;
            --memory)      memory="$2";      shift 2 ;;
            --bandwidth)   bandwidth="$2";   shift 2 ;;
            *) die "Unknown option: $1" ;;
        esac
    done

    local base="$IMGDIR/$distro/current.raw"
    [ -f "$base" ] || die "Base image not found: $base"

    local dir="$INSTDIR/$name"
    [ ! -d "$dir" ] || die "Instance '$name' already exists. Delete it first."
    mkdir -p "$dir"

    log "Creating instance '$name' ($distro)"

    # System disk: sparse raw copy (cloud-hypervisor does not support qcow2 backing files)
    log "  System disk: $system_size (sparse raw copy of $distro base)"
    cp --sparse=always "$base" "$dir/system.raw"
    qemu-img resize -f raw "$dir/system.raw" "$system_size" -q

    # Optional data disk
    if [ -n "$data_size" ]; then
        log "  Data disk: $data_size (raw)"
        qemu-img create -f raw "$dir/data.raw" "$data_size" -q
    fi

    # Save instance config
    cat > "$dir/config" <<EOF
DISTRO=$distro
CPUS=$cpus
MEMORY=$memory
BANDWIDTH=$bandwidth
HAS_DATA=${data_size:+yes}
EOF

    # Copy cmdline
    cp "$IMGDIR/$distro/cmdline" "$dir/cmdline"

    alloc_network "$name"

    gen_cloudinit "$dir" "$name" "$IP" "$MAC" "$distro"

    log "  IP: $IP  MAC: $MAC  TAP: $TAP"
    [ "$bandwidth" -gt 0 ] 2>/dev/null && log "  Bandwidth: ${bandwidth} Mbps"
    log "Done. Start with: $0 start $name"
}

cmd_start() {
    local name="${1:-}"
    [ -n "$name" ] || die "Usage: $0 start <name> [--cpus N] [--memory NM] [--bandwidth N]"

    local dir="$INSTDIR/$name"
    [ -d "$dir" ] || die "Instance '$name' not found"

    local pidfile="$RUNDIR/$name.pid"
    if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        die "Instance '$name' is already running (PID $(cat "$pidfile"))"
    fi

    # Load config
    # shellcheck source=/dev/null
    source "$dir/config"
    alloc_network "$name"

    # Override from args
    while [[ $# -gt 1 ]]; do
        case "$2" in
            --cpus)      CPUS="$3";      shift 2 ;;
            --memory)    MEMORY="$3";    shift 2 ;;
            --bandwidth) BANDWIDTH="$3"; shift 2 ;;
            *) die "Unknown option: $2" ;;
        esac
    done
    BANDWIDTH="${BANDWIDTH:-0}"

    setup_tap

    local vmlinuz="$IMGDIR/$DISTRO/vmlinuz"
    local initrd="$IMGDIR/$DISTRO/initrd"
    local cmdline
    cmdline=$(cat "$dir/cmdline")

    [ -f "$vmlinuz" ] || die "Kernel not found: $vmlinuz"
    [ -f "$initrd"  ] || die "Initrd not found: $initrd"

    # ── Disk args ─────────────────────────────────────────────────────────────
    # System disk: O_DIRECT (bypass host page cache) + multi-queue I/O
    local disk_queues=$CPUS
    [ "$disk_queues" -gt 4 ] && disk_queues=4
    local disk_args="--disk path=$dir/system.raw,direct=on,num_queues=$disk_queues"
    # cloud-init and data disks are read-mostly; skip direct/multi-queue for them
    [ -f "$dir/cloudinit.img" ] && disk_args="$disk_args path=$dir/cloudinit.img,readonly=on"
    [ "${HAS_DATA:-}" = "yes" ] && disk_args="$disk_args path=$dir/data.raw,direct=on"

    # ── CPU args ──────────────────────────────────────────────────────────────
    # max=64 reserves hotplug slots; actual count is boot=CPUS
    local cpu_args="boot=${CPUS},max=64"

    # ── Memory args ───────────────────────────────────────────────────────────
    # mergeable:  KSM page merging — improves density when running many VMs
    # thp:        Transparent Huge Pages — reduces virtualisation page faults
    # hotplug_method=virtio-mem: supports both add and remove (ACPI is add-only)
    # hotplug_size=64G: reserves virtual address space for future resize; no physical cost
    local mem_args="${MEMORY},mergeable=on,thp=on,hotplug_method=virtio-mem,hotplug_size=64G"

    # ── Net args ──────────────────────────────────────────────────────────────
    local net_args="tap=${TAP},mac=${MAC},id=net0"
    local rate
    rate=$(net_rate_args "$BANDWIDTH")
    [ -n "$rate" ] && net_args="${net_args},${rate}"

    log "Starting '$name' ($DISTRO, ${CPUS} vCPU, ${MEMORY}${BANDWIDTH:+, ${BANDWIDTH} Mbps})"
    log "  IP: $IP  (ssh root@$IP)"

    # shellcheck disable=SC2086
    "$BINARY" \
        --kernel    "$vmlinuz" \
        --initramfs "$initrd" \
        --cmdline   "$cmdline" \
        $disk_args \
        --cpus      "$cpu_args" \
        --memory    "size=$mem_args" \
        --net       "$net_args" \
        --balloon   "size=0,deflate_on_oom=on,free_page_reporting=on" \
        --serial    "file=$RUNDIR/$name-serial.log" \
        --console   off \
        --api-socket "$RUNDIR/$name-api.sock" \
        --log-file  "$RUNDIR/$name.log" &

    echo $! > "$pidfile"
    log "  PID: $(cat "$pidfile")   Serial: $RUNDIR/$name-serial.log"

    # Wait for VM to respond
    local waited=0
    while [ "$waited" -lt 60 ]; do
        if ping -c1 -W1 "$IP" &>/dev/null; then
            log "  VM is up! ssh root@$IP"
            return 0
        fi
        sleep 2; ((waited+=2))
    done
    log "  VM did not respond in 60s — check: tail -f $RUNDIR/$name-serial.log"
}

cmd_stop() {
    local name="${1:-}"
    [ -n "$name" ] || die "Usage: $0 stop <name>"

    local pidfile="$RUNDIR/$name.pid"
    [ -f "$pidfile" ] || die "Instance '$name' is not running"

    local pid
    pid=$(cat "$pidfile")
    if kill -0 "$pid" 2>/dev/null; then
        # Graceful shutdown via API, fallback to SIGTERM
        if [ -S "$RUNDIR/$name-api.sock" ]; then
            curl -s --unix-socket "$RUNDIR/$name-api.sock" \
                -X PUT http://localhost/api/v1/vm.shutdown 2>/dev/null || true
            sleep 3
        fi
        kill -0 "$pid" 2>/dev/null && kill "$pid" || true
    fi
    rm -f "$pidfile"
    log "Stopped '$name'"
}

cmd_status() {
    printf "%-16s %-10s %-16s %-8s %-12s %s\n" "NAME" "DISTRO" "IP" "STATUS" "BANDWIDTH" "PID"
    echo "──────────────────────────────────────────────────────────────────"
    for dir in "$INSTDIR"/*/; do
        [ -f "$dir/config" ] || continue
        local name
        name=$(basename "$dir")
        # shellcheck source=/dev/null
        source "$dir/config"
        alloc_network "$name" 2>/dev/null

        local status="stopped" pid="-"
        local pidfile="$RUNDIR/$name.pid"
        if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
            status="running"
            pid=$(cat "$pidfile")
        fi
        local bw_label="${BANDWIDTH:-0}"
        [ "$bw_label" -gt 0 ] 2>/dev/null && bw_label="${bw_label}Mbps" || bw_label="unlimited"
        printf "%-16s %-10s %-16s %-8s %-12s %s\n" \
            "$name" "$DISTRO" "${IP:-?}" "$status" "$bw_label" "$pid"
    done
}

cmd_delete() {
    local name="${1:-}"
    [ -n "$name" ] || die "Usage: $0 delete <name>"

    local pidfile="$RUNDIR/$name.pid"
    if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        die "Instance '$name' is running. Stop it first: $0 stop $name"
    fi

    local dir="$INSTDIR/$name"
    [ -d "$dir" ] || die "Instance '$name' not found"

    # Release TAP
    if [ -f "$dir/network" ]; then
        # shellcheck source=/dev/null
        source "$dir/network"
        ip link delete "$TAP" 2>/dev/null || true
    fi

    rm -rf "$dir"
    rm -f "$RUNDIR/$name"*
    log "Deleted instance '$name'"
}

cmd_resize_system() {
    local name="${1:-}" newsize="${2:-}"
    [ -n "$name" ] && [ -n "$newsize" ] || die "Usage: $0 resize-system <name> <size>  e.g. 30G"

    local pidfile="$RUNDIR/$name.pid"
    if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        die "Stop the VM first: $0 stop $name"
    fi

    local disk="$INSTDIR/$name/system.raw"
    [ -f "$disk" ] || die "Disk not found: $disk"

    local old_size
    old_size=$(qemu-img info "$disk" | awk '/virtual size/{print $3, $4}')
    qemu-img resize "$disk" "$newsize"
    log "Resized $name system disk: $old_size -> $newsize"
    log "NOTE: Resize the partition inside the VM after next boot:"
    log "  Debian/Ubuntu: sudo growpart /dev/vda 1 && sudo resize2fs /dev/vda1"
    log "  Rocky:         sudo growpart /dev/vda 4 && sudo xfs_growfs /"
    log "  Alpine:        sudo resize2fs /dev/vda3"
}

cmd_info() {
    local name="${1:-}"
    [ -n "$name" ] || die "Usage: $0 info <name>"

    local dir="$INSTDIR/$name"
    [ -d "$dir" ] || die "Instance '$name' not found"

    # shellcheck source=/dev/null
    source "$dir/config"
    alloc_network "$name"

    echo "Instance  : $name"
    echo "Distro    : $DISTRO"
    echo "IP        : $IP"
    echo "MAC       : $MAC"
    echo "TAP       : $TAP"
    echo "CPUs      : $CPUS (max hotplug: 64)"
    echo "Memory    : $MEMORY (hotplug up to 64G, virtio-mem)"
    local bw_label="${BANDWIDTH:-0}"
    [ "$bw_label" -gt 0 ] 2>/dev/null && bw_label="${bw_label} Mbps" || bw_label="unlimited"
    echo "Bandwidth : $bw_label"
    echo ""
    echo "Disks:"
    qemu-img info "$dir/system.raw" | grep -E "file|virtual size|disk size"
    [ -f "$dir/data.raw" ] && qemu-img info "$dir/data.raw" | grep -E "file|virtual size|disk size"
    echo ""
    local pidfile="$RUNDIR/$name.pid"
    if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
        echo "Status    : running (PID $(cat "$pidfile"))"
        echo "Serial    : tail -f $RUNDIR/$name-serial.log"
        echo "API sock  : $RUNDIR/$name-api.sock"
    else
        echo "Status    : stopped"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────
cmd="${1:-}"
shift || true

case "$cmd" in
    create)       cmd_create "$@" ;;
    start)        cmd_start  "$@" ;;
    stop)         cmd_stop   "$@" ;;
    status|list)  cmd_status     ;;
    delete)       cmd_delete "$@" ;;
    resize-system) cmd_resize_system "$@" ;;
    info)         cmd_info   "$@" ;;
    *)
        echo "Usage: $0 <command> [options]"
        echo ""
        echo "Commands:"
        echo "  create <name> <distro> [--system-size 20G] [--data-size 50G] [--cpus 2] [--memory 1024M] [--bandwidth 100]"
        echo "  start  <name> [--cpus N] [--memory NM] [--bandwidth N]"
        echo "  stop   <name>"
        echo "  status"
        echo "  delete <name>"
        echo "  resize-system <name> <new-size>"
        echo "  info   <name>"
        echo ""
        echo "Distros: debian alpine"
        exit 1
        ;;
esac
