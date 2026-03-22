#!/usr/bin/env bash
# update-vm-images.sh
# Periodically downloads and prepares the latest VM images for cloud-hypervisor.
# Supports: Debian 13, Alpine (virt)
#
# Usage: ./update-vm-images.sh [--force] [debian|alpine]
#
# Directory layout:
#   $IMGDIR/<distro>/current.raw       - raw disk image
#   $IMGDIR/<distro>/vmlinuz           - kernel
#   $IMGDIR/<distro>/initrd            - initrd
#   $IMGDIR/<distro>/version           - current version string

set -euo pipefail

IMGDIR="${VM_IMAGE_DIR:-/opt/vm-images}"
FORCE=0
TARGETS=()

# ── Parse args ────────────────────────────────────────────────────────────────
for arg in "$@"; do
    case "$arg" in
        --force) FORCE=1 ;;
        debian|alpine) TARGETS+=("$arg") ;;
        *) echo "Unknown argument: $arg"; exit 1 ;;
    esac
done
[ ${#TARGETS[@]} -eq 0 ] && TARGETS=(debian alpine)

# ── Helpers ───────────────────────────────────────────────────────────────────
log()  { echo "[$(date '+%H:%M:%S')] $*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }

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

# ── Main ──────────────────────────────────────────────────────────────────────
require_tools
log "VM image update started. Images dir: $IMGDIR"
log "Targets: ${TARGETS[*]}"

for target in "${TARGETS[@]}"; do
    case "$target" in
        debian) build_debian ;;
        alpine) build_alpine ;;
    esac
    echo
done

log "All done."
log ""
log "To start a VM:"
log "  DISTRO=debian  # or alpine"
log "  ./cloud-hypervisor-static \\"
log "      --kernel \$VM_IMAGE_DIR/\$DISTRO/vmlinuz \\"
log "      --initramfs \$VM_IMAGE_DIR/\$DISTRO/initrd \\"
log "      --cmdline \"\$(cat \$VM_IMAGE_DIR/\$DISTRO/cmdline)\" \\"
log "      --disk path=\$VM_IMAGE_DIR/\$DISTRO/current.raw \\"
log "      --cpus boot=2 --memory size=1024M \\"
log "      --net \"tap=tap0,mac=12:34:56:78:90:ab,ip=192.168.249.1,mask=255.255.255.0\""
