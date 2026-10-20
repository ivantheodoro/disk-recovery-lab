#!/bin/sh
# Regenerates the small disk images in testdata/ (macOS only: needs hdiutil,
# diskutil, newfs_msdos and sips). The images are committed, so contributors
# on other systems never need to run this.
set -eu
cd "$(dirname "$0")/.."
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

png() { # tiny solid-colour PNG: $1 = file, $2 = r g b
python3 - "$@" <<'EOF'
import struct, sys, zlib
out, r, g, b = sys.argv[1], *map(int, sys.argv[2:5])
w, h = 120, 80
raw = b"".join(b"\0" + bytes((r, g, b)) * w for _ in range(h))
ch = lambda t, d: struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
open(out, "wb").write(b"\x89PNG\r\n\x1a\n" + ch(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
                      + ch(b"IDAT", zlib.compress(raw)) + ch(b"IEND", b""))
EOF
}

attach() { hdiutil attach -nomount -imagekey diskimage-class=CRawDiskImage "$1" | awk 'NR==1{print $1}'; }

# --- HFSX (case-sensitive journaled) on GPT, with deleted files ----------
IMG="$T/hfs.img"
mkfile -n 40m "$IMG"
DEV=$(attach "$IMG")
diskutil eraseDisk JHFSX FIXTURE GPT "$DEV" >/dev/null
M=/Volumes/FIXTURE
mkdir -p "$M/Photos" "$M/Documents/Work" "$M/VMs/Test.vmwarevm"
png "$M/Photos/red.png" 220 40 40
png "$M/Photos/green.png" 40 160 70
sips -s format jpeg "$M/Photos/red.png" --out "$M/Photos/red.jpg" >/dev/null
printf 'config.version = "8"\nmemsize = "2048"\n' > "$M/VMs/Test.vmwarevm/Test.vmx"
printf 'quarterly numbers\n%.0s' $(seq 1 300) > "$M/Documents/Work/report.txt"
printf '%%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%%%EOF\n' > "$M/Documents/deleted.pdf"
png "$M/Photos/deleted-blue.png" 40 90 220
# Make sure the data reaches the image before deleting, as on a real disk.
sync; diskutil unmount "$M" >/dev/null; diskutil mount "${DEV}s1" >/dev/null
rm "$M/Documents/deleted.pdf" "$M/Photos/deleted-blue.png"
sync
diskutil unmountDisk "$DEV" >/dev/null
hdiutil detach "$DEV" >/dev/null
gzip -9 -c "$IMG" > testdata/hfs.img.gz

# --- FAT32 whole-disk volume with the boot sector wiped -------------------
IMG="$T/fat.img"
mkfile -n 40m "$IMG"
DEV=$(attach "$IMG")
newfs_msdos -F 32 -v FIXFAT "$(echo "$DEV" | sed 's|/dev/|/dev/r|')" >/dev/null
diskutil mount -mountPoint "$T/m" "$DEV" >/dev/null 2>&1 || { mkdir -p "$T/m"; diskutil mount -mountPoint "$T/m" "$DEV" >/dev/null; }
png "$T/m/kept.png" 200 200 30
png "$T/m/gone.png" 30 200 200
sync; diskutil unmount "$T/m" >/dev/null; diskutil mount -mountPoint "$T/m" "$DEV" >/dev/null
rm "$T/m/gone.png"; sync
diskutil unmount "$T/m" >/dev/null
hdiutil detach "$DEV" >/dev/null
dd if=/dev/zero of="$IMG" bs=1024 count=1 conv=notrunc 2>/dev/null
gzip -9 -c "$IMG" > testdata/fat32-wiped.img.gz

ls -l testdata/
