#!/usr/bin/env bash
set -euo pipefail

# Installer satu langkah VodLocal untuk mini PC Linux (Ubuntu/Debian + systemd).
#
# Cukup bawa satu file ini ke setiap mini PC:
#   sudo bash install-linux.sh
#   sudo -E ADMIN_PASSWORD='password-kuat' bash install-linux.sh
#
# Tanpa clone manual (repo publik):
#   curl -fsSL -o /tmp/vodlocal-install.sh \
#     https://raw.githubusercontent.com/Developer-Digdaya/VodLocal-GO/main/scripts/install-linux.sh
#   sudo bash /tmp/vodlocal-install.sh
#
# Yang dikerjakan:
#   1. pasang paket dasar (git, curl, openssl, iproute2, exFAT/NTFS)
#   2. clone repo, atau git pull bila sudah ada
#   3. pasang Go sesuai go.mod + symlink /usr/local/bin/go (langsung bisa dipanggil)
#   4. bebaskan port 80 (nginx/apache2 dimatikan) lalu scripts/deploy-linux.sh
#   5. scripts/ubuntu-usb-automount.sh -> auto-mount flashdisk/HDD USB
#
# Skrip ini idempoten: jalankan ulang untuk update (git pull + build + restart).
#
# Override environment:
#   REPO_URL             default: https://github.com/Developer-Digdaya/VodLocal-GO.git
#   BRANCH               default: main
#   APP_DIR              default: /var/www/vodlocal
#   GITHUB_TOKEN         token baca-saja untuk repo privat; tidak disimpan di .git/config
#   GO_VERSION           default: 1.27.1 (sama dengan go.mod; naik hanya bila
#                        go.mod meminta lebih baru, tidak mengejar versi terbaru)
#   GO_BIN               path ke binary go yang sudah ada
#   SKIP_BUILD=1         pakai binary $APP_DIR/bin/vodlocal yang sudah ada
#   SKIP_USB_AUTOMOUNT=1 jangan pasang auto-mount USB
#   ADMIN_USER, ADMIN_PASSWORD, SESSION_SECRET, DRAMAHITS_BASE_URL,
#   DENSGO_BASE_URL, SKIP_BACKUP  diteruskan ke deploy-linux.sh

log()  { printf '\n==> %s\n' "$*"; }
warn() { printf 'PERINGATAN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

if [ "$0" = "bash" ] || [ ! -f "$0" ]; then
  die "simpan skrip ini ke file lalu jalankan: sudo bash install-linux.sh"
fi

PASSTHROUGH=(
  REPO_URL BRANCH APP_DIR GITHUB_TOKEN GO_VERSION GO_BIN
  ADMIN_USER ADMIN_PASSWORD SESSION_SECRET DRAMAHITS_BASE_URL DENSGO_BASE_URL
  SKIP_BUILD SKIP_BACKUP SKIP_USB_AUTOMOUNT
)

if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "jalankan sebagai root: sudo bash $0"
  env_args=()
  for name in "${PASSTHROUGH[@]}"; do
    if [ -n "${!name+x}" ]; then
      env_args+=("$name=${!name}")
    fi
  done
  log "Meminta hak root lewat sudo"
  exec sudo env "${env_args[@]}" bash "$0" "$@"
fi

REPO_URL="${REPO_URL:-https://github.com/Developer-Digdaya/VodLocal-GO.git}"
BRANCH="${BRANCH:-main}"
APP_DIR="${APP_DIR:-/var/www/vodlocal}"
GO_VERSION="${GO_VERSION:-1.27.1}"
APP_DIR="${APP_DIR%/}"
RUN_USER="${SUDO_USER:-root}"

command -v systemctl >/dev/null 2>&1 || die "systemctl tidak ditemukan; skrip ini butuh systemd"
if [ "$RUN_USER" != "root" ]; then
  id -u "$RUN_USER" >/dev/null 2>&1 || die "user $RUN_USER tidak ditemukan"
fi

as_user() {
  # GIT_TERMINAL_PROMPT=0: repo privat tanpa token harus gagal cepat, bukan
  # menggantung menunggu prompt username/password.
  if [ "$RUN_USER" = "root" ]; then
    env GIT_TERMINAL_PROMPT=0 "$@"
  else
    sudo -u "$RUN_USER" -H -- env GIT_TERMINAL_PROMPT=0 "$@"
  fi
}

wait_apt_lock() {
  command -v fuser >/dev/null 2>&1 || return 0
  local waited=0
  while fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock >/dev/null 2>&1; do
    if [ "$waited" -ge 120 ]; then
      warn "lock apt masih dipakai setelah 120 detik; melanjutkan"
      return 0
    fi
    if [ "$waited" -eq 0 ]; then
      log "Menunggu lock apt (unattended-upgrades masih berjalan)"
    fi
    sleep 2
    waited=$((waited + 2))
  done
}

ver_at_least() {
  [ -n "${1:-}" ] || return 1
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n 1)" = "$2" ]
}

go_version() {
  "$1" version 2>/dev/null | awk '{print $3}' | sed 's/^go//' || true
}

wait_health() {
  local addr host port
  addr="$(sed -n 's/^VODLOCAL_HTTP_ADDR=//p' "$APP_DIR/vodlocal.env" 2>/dev/null | tail -n 1)"
  addr="${addr:-:80}"
  port="${addr##*:}"
  host="${addr%:*}"
  case "$host" in
    ""|"0.0.0.0"|"[::]") host="127.0.0.1" ;;
  esac
  for _ in $(seq 1 20); do
    if curl -fsS --max-time 2 "http://$host:$port/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  journalctl -u vodlocal -n 40 --no-pager || true
  die "service vodlocal tidak sehat setelah restart"
}

setup_go_env() {
  [ -x /usr/local/go/bin/go ] || return 0
  # /usr/local/bin sudah ada di PATH semua shell dan secure_path sudo, jadi
  # 'go' langsung bisa dipanggil tanpa edit bashrc dan tanpa source apa pun.
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  cat > /etc/profile.d/golang.sh <<'PROFILE'
# Dibuat otomatis oleh scripts/install-linux.sh (VodLocal).
export GOROOT=/usr/local/go
export PATH="$GOROOT/bin:$PATH"
PROFILE
  chmod 0644 /etc/profile.d/golang.sh
  log "Go langsung bisa dipanggil: /usr/local/bin/go -> /usr/local/go/bin/go"
  log "GOROOT/PATH juga tersedia lewat /etc/profile.d/golang.sh"
}

install_go() {
  local arch tmp backup
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "arsitektur $(uname -m) belum didukung; set GO_BIN=/path/ke/go" ;;
  esac
  log "Memasang Go $GO_VERSION ($arch) ke /usr/local/go"
  tmp="$(mktemp -d)"
  if ! curl -fsSL --retry 3 --retry-delay 2 \
      -o "$tmp/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; then
    rm -rf "$tmp"
    die "unduh Go $GO_VERSION gagal; butuh koneksi internet"
  fi
  if [ -d /usr/local/go ]; then
    backup="/usr/local/go.old-$(date +%Y%m%d%H%M%S)"
    mv /usr/local/go "$backup"
    log "Go lama dipindahkan ke $backup"
  fi
  if ! tar -C /usr/local -xzf "$tmp/go.tar.gz"; then
    rm -rf "$tmp"
    die "ekstrak Go gagal"
  fi
  rm -rf "$tmp"
  export PATH="/usr/local/go/bin:$PATH"
  GO_BIN=/usr/local/go/bin/go
  ver_at_least "$(go_version "$GO_BIN")" "$REQUIRED_GO" ||
    die "Go terpasang tidak memenuhi kebutuhan go.mod ($REQUIRED_GO)"
}

# 1. Paket dasar
missing=()
optional=()
add_pkg() {
  local p
  for p in "${missing[@]}"; do
    if [ "$p" = "$1" ]; then
      return 0
    fi
  done
  missing+=("$1")
}
add_optional() {
  local p
  for p in "${optional[@]}"; do
    if [ "$p" = "$1" ]; then
      return 0
    fi
  done
  optional+=("$1")
}
need() { command -v "$1" >/dev/null 2>&1 || add_pkg "$2"; }
pkg_installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"; }

need git               git
need curl              curl
need openssl           openssl
need ss                iproute2
need logger            bsdutils
need mount             util-linux
need umount            util-linux
need findmnt           util-linux
need lsblk             util-linux
need blkid             util-linux
need mountpoint        util-linux
need udevadm           udev
need systemd-tmpfiles  systemd
[ -f /etc/ssl/certs/ca-certificates.crt ] || add_pkg ca-certificates
# Kernel 5.15+ sudah punya driver exfat/ntfs3; paket ini untuk fsck dan fallback.
pkg_installed exfatprogs || add_optional exfatprogs
pkg_installed ntfs-3g    || add_optional ntfs-3g

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a

if [ "${#missing[@]}" -gt 0 ]; then
  command -v apt-get >/dev/null 2>&1 ||
    die "paket hilang (${missing[*]}) dan apt-get tidak ada; distro ini belum didukung"
  log "Memasang paket: ${missing[*]}"
  wait_apt_lock
  apt-get update -qq || die "apt-get update gagal; butuh koneksi internet"
  wait_apt_lock
  apt-get install -y -qq --no-install-recommends "${missing[@]}" || die "apt-get install gagal"
fi

if [ "${#optional[@]}" -gt 0 ] && command -v apt-get >/dev/null 2>&1; then
  log "Memasang paket opsional USB: ${optional[*]}"
  wait_apt_lock
  apt-get install -y -qq --no-install-recommends "${optional[@]}" ||
    warn "paket opsional gagal dipasang; mount exFAT/NTFS tetap jalan lewat driver kernel"
fi

# 2. Repo
clone_opts=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  auth="$(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64 -w0)"
  clone_opts=(-c "http.extraHeader=Authorization: Basic $auth")
fi

if [ -d "$APP_DIR/.git" ]; then
  log "Repo sudah ada di $APP_DIR; memperbarui ke origin/$BRANCH"
  as_user git -C "$APP_DIR" "${clone_opts[@]}" fetch --prune origin ||
    die "git fetch gagal. Repo privat? set GITHUB_TOKEN atau pakai REPO_URL SSH."
  as_user git -C "$APP_DIR" checkout "$BRANCH" ||
    die "checkout $BRANCH gagal; selesaikan perubahan lokal di $APP_DIR dulu"
  as_user git -C "$APP_DIR" pull --ff-only origin "$BRANCH" ||
    die "git pull --ff-only gagal; selesaikan perubahan lokal di $APP_DIR dulu"
else
  if [ -e "$APP_DIR" ] && [ -n "$(ls -A "$APP_DIR" 2>/dev/null || true)" ]; then
    die "$APP_DIR sudah ada dan bukan repo git; pindahkan atau set APP_DIR lain"
  fi
  log "Clone $REPO_URL ke $APP_DIR"
  mkdir -p "$APP_DIR"
  chown "$RUN_USER" "$APP_DIR"
  as_user git "${clone_opts[@]}" clone --branch "$BRANCH" --single-branch "$REPO_URL" "$APP_DIR" ||
    die "git clone gagal. Repo publik? bila privat set GITHUB_TOKEN=... atau REPO_URL SSH."
fi

[ -f "$APP_DIR/go.mod" ] || die "go.mod tidak ditemukan di $APP_DIR"
[ -f "$APP_DIR/scripts/deploy-linux.sh" ] || die "scripts/deploy-linux.sh tidak ditemukan di $APP_DIR"
[ -f "$APP_DIR/scripts/ubuntu-usb-automount.sh" ] || die "scripts/ubuntu-usb-automount.sh tidak ditemukan di $APP_DIR"

# 3. Go (hanya bila build dilakukan di mesin ini)
REQUIRED_GO="$(awk '$1 == "go" { print $2; exit }' "$APP_DIR/go.mod")"
[ -n "$REQUIRED_GO" ] || die "tidak bisa membaca versi Go dari go.mod"

if ver_at_least "$REQUIRED_GO" "$GO_VERSION"; then
  GO_VERSION="$REQUIRED_GO"
fi

GO_BIN="${GO_BIN:-}"
if [ -n "$GO_BIN" ]; then
  [ -x "$GO_BIN" ] || die "GO_BIN tidak executable: $GO_BIN"
elif [ "${SKIP_BUILD:-0}" = "1" ]; then
  log "SKIP_BUILD=1; memakai binary yang sudah ada di $APP_DIR/bin/vodlocal"
else
  for candidate in "$(command -v go 2>/dev/null || true)" \
                   /usr/local/go/bin/go /usr/lib/go/bin/go /snap/bin/go; do
    [ -n "$candidate" ] && [ -x "$candidate" ] || continue
    if ver_at_least "$(go_version "$candidate")" "$REQUIRED_GO"; then
      GO_BIN="$candidate"
      break
    fi
  done
  if [ -z "$GO_BIN" ]; then
    avail_kb="$(df -Pk /usr/local 2>/dev/null | awk 'NR == 2 { print $4 }')"
    if [ -n "${avail_kb:-}" ] && [ "$avail_kb" -lt 2097152 ]; then
      warn "ruang kosong /usr/local < 2 GB; unduh Go dan cache build bisa gagal"
    fi
    install_go
  else
    log "Memakai Go $("$GO_BIN" version | awk '{print $3}') di $GO_BIN"
  fi
fi

if [ -n "${GO_BIN:-}" ]; then
  export GO_BIN
fi

setup_go_env

# 4. Bebaskan port 80, lalu deploy aplikasi
for unit in nginx apache2 httpd; do
  if systemctl cat "$unit.service" >/dev/null 2>&1; then
    if systemctl is-active --quiet "$unit" || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
      log "Menonaktifkan $unit (port 80 dipakai VodLocal)"
      systemctl disable --now "$unit" || warn "gagal menonaktifkan $unit"
    fi
  fi
done

if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  log "ufw aktif; membuka port 80/tcp untuk tablet di LAN"
  ufw allow 80/tcp >/dev/null || warn "gagal membuka port 80/tcp di ufw"
fi

log "Menjalankan scripts/deploy-linux.sh"
cd "$APP_DIR"
bash scripts/deploy-linux.sh

# 5. Auto-mount USB (butuh user vodlocal dari langkah sebelumnya)
if [ "${SKIP_USB_AUTOMOUNT:-0}" = "1" ]; then
  log "SKIP_USB_AUTOMOUNT=1; melewati auto-mount USB"
else
  log "Memasang auto-mount USB"
  if ! bash scripts/ubuntu-usb-automount.sh; then
    warn "auto-mount USB gagal; VodLocal tetap terpasang dan sehat"
    warn "perbaiki lalu jalankan: sudo bash $APP_DIR/scripts/ubuntu-usb-automount.sh"
  fi
fi

if [ -f /etc/systemd/system/vodlocal.service.d/50-vodlocal-usb.conf ]; then
  log "Restart vodlocal agar drop-in ReadWritePaths USB terbaca"
  systemctl restart vodlocal
  wait_health
fi

log "Selesai. Jalankan ulang skrip ini kapan saja untuk update."
