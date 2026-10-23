#!/usr/bin/env bash
# Installer auto-mount USB flashdisk/HDD untuk Ubuntu (systemd + udev).
#
# Hasil:
#   - setiap partisi USB dengan filesystem exFAT/VFAT/NTFS di-mount read-write
#     ke /run/media/vodlocal/<uuid>
#   - aplikasi vodlocal (user vodlocal) dapat membaca dan menulis mount tersebut
#   - disk di-unmount saat dicabut
#
# Jalankan sekali dengan sudo:
#   sudo bash scripts/ubuntu-usb-automount.sh
#
# Ini TIDAK melakukan auto-import. User tetap memilih konten di panel admin.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "jalankan dengan sudo" >&2; exit 1; }

MOUNT_ROOT="${VODLOCAL_USB_MOUNT_ROOT:-/run/media/vodlocal}"
APP_USER="${VODLOCAL_USB_USER:-vodlocal}"
CONFIG_FILE="/etc/default/vodlocal-usb"
RULE_FILE="/etc/udev/rules.d/99-vodlocal-usb.rules"
TMPFILES_FILE="/etc/tmpfiles.d/vodlocal-usb.conf"
MOUNT_HELPER="/usr/local/sbin/vodlocal-usb-mount"
UNMOUNT_HELPER="/usr/local/sbin/vodlocal-usb-unmount"
MOUNT_UNIT="/etc/systemd/system/vodlocal-usb-mount@.service"
UNMOUNT_UNIT="/etc/systemd/system/vodlocal-usb-unmount@.service"
SERVICE_DROPIN="/etc/systemd/system/vodlocal.service.d/50-vodlocal-usb.conf"

for cmd in mount umount mountpoint findmnt lsblk blkid udevadm logger systemd-tmpfiles systemctl; do
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "ERROR: '$cmd' tidak ditemukan; instal util-linux/udev" >&2
    exit 1
  }
done

if ! getent passwd "$APP_USER" >/dev/null 2>&1; then
  echo "ERROR: user '$APP_USER' tidak ada. Jalankan deploy-linux.sh dulu." >&2
  exit 1
fi

if [ ! -d "$MOUNT_ROOT" ]; then
  install -d -m 0755 "$MOUNT_ROOT"
fi

# /run adalah tmpfs, jadi mount root harus dibuat ulang setiap boot. Tanpa ini
# ReadWritePaths= bisa membuat service gagal start setelah reboot.
cat > "$TMPFILES_FILE" <<EOF
d $MOUNT_ROOT 0755 root root -
EOF
chmod 0644 "$TMPFILES_FILE"
systemd-tmpfiles --create "$TMPFILES_FILE"

# Konfigurasi yang dibaca helper (bukan disisipkan ke dalam shell helper).
cat > "$CONFIG_FILE" <<EOF
VODLOCAL_USB_MOUNT_ROOT=$MOUNT_ROOT
VODLOCAL_USB_USER=$APP_USER
EOF
chmod 0644 "$CONFIG_FILE"

cat > "$MOUNT_HELPER" <<'HELPER'
#!/usr/bin/env bash
set -euo pipefail

# Dipanggil udev (RUN) atau systemd (mount unit) sebagai root dengan argumen
# /dev/<partition>.
. /etc/default/vodlocal-usb

DEV="${1:-}"
[ -n "$DEV" ] || exit 0
[ -b "$DEV" ] || exit 0

log() { logger -t vodlocal-usb -- "$*" 2>/dev/null || true; }

# Properti ID_FS_* hanya ada saat dipanggil udev. Saat dipanggil systemd,
# probe sendiri supaya helper tidak bergantung pada environment udev.
FSTYPE="${ID_FS_TYPE:-}"
if [ -z "$FSTYPE" ]; then
  FSTYPE="$(blkid -o value -s TYPE "$DEV" 2>/dev/null || true)"
fi
case "$FSTYPE" in
  exfat|vfat|ntfs|ntfs3) ;;
  *) exit 0 ;; # bukan filesystem yang didukung
esac

# Jangan mount perangkat yang sudah di-mount (mis. disk root USB).
if findmnt -rn -S "$DEV" -o TARGET 2>/dev/null | grep -q .; then
  exit 0
fi

NAME="${ID_FS_UUID:-}"
if [ -z "$NAME" ]; then
  NAME="${ID_FS_LABEL:-}"
fi
if [ -z "$NAME" ]; then
  NAME="$(blkid -o value -s UUID "$DEV" 2>/dev/null || true)"
fi
if [ -z "$NAME" ]; then
  NAME="$(blkid -o value -s LABEL "$DEV" 2>/dev/null || true)"
fi
if [ -z "$NAME" ]; then
  NAME="dev-${DEV##*/}"
fi
# mountpoint harus aman: hanya huruf, angka, titik, underscore, dash.
NAME="$(printf '%s' "$NAME" | tr -c 'A-Za-z0-9._-' '-')"
NAME="${NAME#-}"
[ -n "$NAME" ] || exit 0

UID_APP="$(id -u "$VODLOCAL_USB_USER" 2>/dev/null || echo 0)"
GID_APP="$(id -g "$VODLOCAL_USB_USER" 2>/dev/null || echo 0)"
MOUNTPOINT="$VODLOCAL_USB_MOUNT_ROOT/$NAME"
mkdir -p "$MOUNTPOINT"

if mountpoint -q "$MOUNTPOINT" 2>/dev/null; then
  exit 0
fi

BASE="rw,nosuid,nodev,noexec,noatime"
case "$FSTYPE" in
  exfat|vfat|ntfs|ntfs3)
    OPTS="$BASE,uid=$UID_APP,gid=$GID_APP,umask=0022"
    ;;
  *)
    OPTS="$BASE"
    ;;
esac

mount_rw() {
  local type="$1"
  if mount -t "$type" -o "$OPTS" "$DEV" "$MOUNTPOINT" 2>/dev/null; then
    return 0
  fi
  return 1
}

mounted=0
case "$FSTYPE" in
  ntfs)
    mount_rw ntfs3 && mounted=1 || true
    [ "$mounted" = 1 ] || mount_rw ntfs && mounted=1 || true
    ;;
  *)
    mount_rw "$FSTYPE" && mounted=1 || true
    ;;
esac

if [ "$mounted" != 1 ]; then
  # Jangan biarkan disk tidak terlihat sama sekali: fallback read-only untuk import.
  log "mount rw gagal untuk $DEV ($FSTYPE), mencoba read-only"
  if mount -t "$FSTYPE" -o "ro,nosuid,nodev,noexec,noatime" "$DEV" "$MOUNTPOINT" 2>/dev/null; then
    log "mount read-only: $MOUNTPOINT"
    exit 0
  fi
  rmdir "$MOUNTPOINT" 2>/dev/null || true
  log "gagal mount $DEV ($FSTYPE)"
  exit 0
fi

log "mount rw: $DEV -> $MOUNTPOINT"
HELPER
chmod 0755 "$MOUNT_HELPER"

cat > "$UNMOUNT_HELPER" <<'HELPER'
#!/usr/bin/env bash
set -euo pipefail
. /etc/default/vodlocal-usb

UUID=""
DEV=""
ARG="${1:-}"
if [ -n "$ARG" ] && [ "${ARG#/dev/}" != "$ARG" ]; then
  DEV="$ARG"
else
  UUID="$ARG"
fi
if [ -z "$DEV" ] && [ -n "${2:-}" ]; then
  DEV="$2"
fi
[ -n "$UUID" ] || [ -n "$DEV" ] || exit 0
UUID="$(printf '%s' "$UUID" | tr -c 'A-Za-z0-9._-' '-')"
UUID="${UUID#-}"

MOUNTPOINT=""
if [ -n "$UUID" ]; then
  MOUNTPOINT="$VODLOCAL_USB_MOUNT_ROOT/$UUID"
fi
if { [ -z "$MOUNTPOINT" ] || ! mountpoint -q "$MOUNTPOINT" 2>/dev/null; } && [ -n "$DEV" ]; then
  FALLBACK="$(findmnt -rn -S "$DEV" -o TARGET 2>/dev/null | head -n 1 || true)"
  case "$FALLBACK" in
    "$VODLOCAL_USB_MOUNT_ROOT"/*) MOUNTPOINT="$FALLBACK" ;;
  esac
fi
[ -n "$MOUNTPOINT" ] || exit 0
log() { logger -t vodlocal-usb -- "$*" 2>/dev/null || true; }

if mountpoint -q "$MOUNTPOINT" 2>/dev/null; then
  sync
  if ! umount "$MOUNTPOINT" 2>/dev/null; then
    # Disk sudah tercabut atau masih sibuk. Lazy unmount mencegah mount basi.
    umount -l "$MOUNTPOINT" 2>/dev/null || true
  fi
  log "unmount: $MOUNTPOINT"
fi
rmdir "$MOUNTPOINT" 2>/dev/null || true
HELPER
chmod 0755 "$UNMOUNT_HELPER"

# Mount tidak boleh dijalankan langsung dari RUN{program}: systemd-udevd berjalan
# di mount namespace sendiri (PrivateMounts=yes), sehingga hasil mount tidak
# terlihat oleh host maupun service vodlocal. Unit template berikut dijalankan
# oleh systemd di namespace host.
cat > "$MOUNT_UNIT" <<'UNIT'
[Unit]
Description=VodLocal mount USB partition /dev/%i
After=dev-%i.device

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/vodlocal-usb-mount /dev/%i
UNIT
chmod 0644 "$MOUNT_UNIT"

cat > "$UNMOUNT_UNIT" <<'UNIT'
[Unit]
Description=VodLocal unmount USB partition /dev/%i

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/vodlocal-usb-unmount /dev/%i
UNIT
chmod 0644 "$UNMOUNT_UNIT"

# Semua partisi USB; helper sendiri yang menolak filesystem yang tidak didukung.
# SUBSYSTEMS=="usb" menelusuri rantai parent sehingga tidak bergantung pada
# properti ID_BUS yang bisa berbeda antar versi udev.
cat > "$RULE_FILE" <<'RULE'
ACTION=="add", SUBSYSTEM=="block", SUBSYSTEMS=="usb", RUN{program}+="/bin/systemctl --no-block start vodlocal-usb-mount@%k.service"
ACTION=="remove", SUBSYSTEM=="block", SUBSYSTEMS=="usb", RUN{program}+="/bin/systemctl --no-block start vodlocal-usb-unmount@%k.service"
RULE
chmod 0644 "$RULE_FILE"

# Agar service yang memakai ProtectSystem=strict boleh menulis ke mount USB.
if [ -f /etc/systemd/system/vodlocal.service ]; then
  install -d -m 0755 "$(dirname "$SERVICE_DROPIN")"
  cat > "$SERVICE_DROPIN" <<EOF
[Service]
ReadWritePaths=-$MOUNT_ROOT
EOF
  chmod 0644 "$SERVICE_DROPIN"
fi

systemctl daemon-reload
udevadm control --reload-rules
udevadm trigger --subsystem-match=block --action=add
udevadm settle --timeout=10 2>/dev/null || true

# Mount partisi USB yang sudah tercolok saat installer dijalankan. Deteksi
# memakai TRAN disk induk (lsblk) dan helper mem-probe filesystem sendiri,
# jadi tidak bergantung pada event udev maupun properti ID_*.
while IFS= read -r dev; do
  [ -b "$dev" ] || continue
  parent="$(lsblk -no PKNAME "$dev" 2>/dev/null | head -n 1 | tr -d '[:space:]' || true)"
  if [ -n "$parent" ]; then
    tran="$(lsblk -no TRAN "/dev/$parent" 2>/dev/null | head -n 1 | tr -d '[:space:]' || true)"
  else
    tran="$(lsblk -no TRAN "$dev" 2>/dev/null | head -n 1 | tr -d '[:space:]' || true)"
  fi
  [ "$tran" = "usb" ] || continue
  printf 'Mounting %s ... ' "$dev"
  if systemctl start "vodlocal-usb-mount@${dev##*/}.service"; then
    echo "selesai"
  else
    echo "GAGAL"
  fi
done < <(lsblk -nrpo NAME 2>/dev/null)

cat <<EOF

Auto-mount USB terpasang.
  Mount root  : $MOUNT_ROOT
  App user    : $APP_USER
  Helper      : $MOUNT_HELPER
  Unit mount  : $MOUNT_UNIT
  Unit unmount: $UNMOUNT_UNIT
  Rule        : $RULE_FILE

Partisi USB yang sudah tercolok langsung di-mount oleh installer ini.
Colokkan flashdisk/HDD. Mount read-write akan muncul sebagai:
  $MOUNT_ROOT/<uuid>

Jika drive sudah ada sebelum installer dijalankan, cabut dan colok ulang.
Jika service vodlocal sudah berjalan sebelum installer ini, restart sekali agar
drop-in ReadWritePaths ikut terpasang:
  sudo systemctl restart vodlocal

Jika drive tidak terlihat di panel admin:
  - cek mount: findmnt -rn -S /dev/sdX -o TARGET
  - cek log helper: journalctl -t vodlocal-usb -n 50 --no-pager
  - cek unit: systemctl status 'vodlocal-usb-mount@*.service'
  - cek service: systemctl show vodlocal -p ReadWritePaths
EOF
