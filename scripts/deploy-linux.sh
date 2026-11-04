#!/usr/bin/env bash
set -euo pipefail

# Deploy VodLocal di Linux dengan systemd.
#
# Pemakaian:
#   sudo bash scripts/deploy-linux.sh
#   sudo -E DISABLE_NGINX=1 ADMIN_PASSWORD='...' bash scripts/deploy-linux.sh
#
# Override environment:
#   APP_DIR              lokasi instalasi (default: root repo)
#   ADMIN_USER           default: admin
#   ADMIN_PASSWORD       kosong = generate acak
#   SESSION_SECRET       kosong = generate acak
#   DRAMAHITS_BASE_URL   default: URL produksi di deploy/vodlocal.env.example
#   DENSGO_BASE_URL      default: URL publik Densgo (movie & series); lihat
#                        deploy/vodlocal.env.example
#   GO_BIN               path ke binary go (untuk sudo dengan secure_path)
#   SKIP_BUILD=1         jangan build, pakai binary yang sudah ada
#   SKIP_BACKUP=1        jangan backup database sebelum restart
#   DISABLE_NGINX=1      hentikan dan disable nginx (port 80)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_DIR="${APP_DIR:-$ROOT}"
BINARY="$APP_DIR/bin/vodlocal"
ENV_FILE="$APP_DIR/vodlocal.env"
SERVICE_FILE="/etc/systemd/system/vodlocal.service"

# Default URL publik Densgo (movie & series). Nilai env yang sudah ada tidak diubah;
# variabel ini hanya dipakai saat env baru dibuat atau saat baris Densgo belum ada.
DENSGO_BASE_URL="${DENSGO_BASE_URL:-https://pp.dens.tv:777/index.php/v3070/adps_list_movies_v1}"

# Default API Dramahits produksi (tab Short). Sama seperti Densgo, nilai env yang
# sudah ada tidak diubah; lihat blok upgrade di bawah untuk URL staging lama.
DRAMAHITS_BASE_URL="${DRAMAHITS_BASE_URL:-https://microdrama.digdaya.co.id/api/v1}"

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "jalankan dengan sudo: sudo bash $0"
[ -f "$ROOT/go.mod" ] || die "go.mod tidak ditemukan di $ROOT"
[ -f "$ROOT/deploy/vodlocal.service" ] || die "deploy/vodlocal.service tidak ditemukan"
[ -f "$ROOT/deploy/vodlocal.env.example" ] || die "deploy/vodlocal.env.example tidak ditemukan"
command -v systemctl >/dev/null 2>&1 || die "systemctl tidak ditemukan"
command -v ss >/dev/null 2>&1 || die "ss tidak ditemukan (paket iproute2)"
command -v curl >/dev/null 2>&1 || die "curl tidak ditemukan (apt install curl)"
command -v openssl >/dev/null 2>&1 || die "openssl tidak ditemukan (apt install openssl)"

log "Repo     : $ROOT"
log "App dir  : $APP_DIR"

mkdir -p "$APP_DIR/bin" "$APP_DIR/data"

# 1. Binary
find_go() {
  local candidate
  if [ -n "${GO_BIN:-}" ] && [ -x "${GO_BIN}" ]; then
    printf '%s\n' "$GO_BIN"
    return 0
  fi
  if command -v go >/dev/null 2>&1; then
    command -v go
    return 0
  fi
  if [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
    candidate="$(sudo -u "$SUDO_USER" -H bash -lc 'command -v go' 2>/dev/null | tail -n 1)"
    if [ -n "$candidate" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  fi
  for candidate in /usr/local/go/bin/go /usr/lib/go/bin/go /snap/bin/go; do
    if [ -x "$candidate" ]; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

if [ "${SKIP_BUILD:-0}" != "1" ]; then
  FOUND_GO="$(find_go || true)"
  if [ -n "$FOUND_GO" ]; then
    log "Build binary Linux dengan $FOUND_GO"
    build_ok=0
    if [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
      build_dir="$(mktemp -d)"
      chown "$SUDO_USER" "$build_dir"
      if (cd "$ROOT" && sudo -u "$SUDO_USER" -H env CGO_ENABLED=0 "$FOUND_GO" build -trimpath -ldflags="-s -w" -o "$build_dir/vodlocal" ./cmd/server); then
        install -m 0755 "$build_dir/vodlocal" "$BINARY"
        build_ok=1
      fi
      rm -rf "$build_dir"
    fi
    if [ "$build_ok" -ne 1 ]; then
      (cd "$ROOT" && CGO_ENABLED=0 "$FOUND_GO" build -trimpath -ldflags="-s -w" -o "$BINARY" ./cmd/server)
    fi
  elif [ -x "$BINARY" ]; then
    log "Go tidak tersedia; memakai binary yang sudah ada"
  else
    die "Go tidak tersedia dan $BINARY belum ada. Build di mesin lain lalu salin ke path tersebut, atau set GO_BIN=/path/ke/go."
  fi
fi

[ -x "$BINARY" ] || die "binary tidak executable: $BINARY"
if command -v file >/dev/null 2>&1; then
  file "$BINARY" | grep -q 'ELF 64-bit' || die "binary bukan ELF Linux 64-bit: $BINARY"
fi
chmod 755 "$BINARY"

# 2. Environment
GENERATED_PASSWORD=0
if [ -f "$ENV_FILE" ]; then
  log "Env sudah ada: $ENV_FILE (nilai tidak ditimpa)"
  # Upgrade dari versi lama: pastikan variabel Densgo ada tanpa mengubah nilai lain.
  if ! grep -q '^VODLOCAL_DENSGO_BASE_URL=' "$ENV_FILE"; then
    {
      printf '\n# Ditambahkan otomatis oleh deploy-linux.sh untuk provider Densgo (movie & series).\n'
      printf 'VODLOCAL_DENSGO_BASE_URL=%s\n' "$DENSGO_BASE_URL"
    } >> "$ENV_FILE"
    log "Menambahkan VODLOCAL_DENSGO_BASE_URL ke env yang sudah ada"
  fi

  # Upgrade dari versi lama: URL Dramahits staging -> produksi. Hanya baris yang
  # masih berisi default lama yang diubah; URL kustom tidak disentuh.
  if grep -qx 'VODLOCAL_DRAMAHITS_BASE_URL=https://stage-microdrama.digdaya.co.id/api/v1' "$ENV_FILE"; then
    sed -i "s#^VODLOCAL_DRAMAHITS_BASE_URL=https://stage-microdrama.digdaya.co.id/api/v1\$#VODLOCAL_DRAMAHITS_BASE_URL=$DRAMAHITS_BASE_URL#" "$ENV_FILE"
    log "Mengganti VODLOCAL_DRAMAHITS_BASE_URL staging ke $DRAMAHITS_BASE_URL"
  fi
  if ! grep -q '^VODLOCAL_DRAMAHITS_BASE_URL=' "$ENV_FILE"; then
    printf 'VODLOCAL_DRAMAHITS_BASE_URL=%s\n' "$DRAMAHITS_BASE_URL" >> "$ENV_FILE"
    log "Menambahkan VODLOCAL_DRAMAHITS_BASE_URL ke env yang sudah ada"
  fi
else
  log "Membuat env: $ENV_FILE"
  SESSION_SECRET="${SESSION_SECRET:-$(openssl rand -hex 32)}"
  ADMIN_USER="${ADMIN_USER:-admin}"
  if [ -z "${ADMIN_PASSWORD:-}" ] && [ -t 0 ]; then
    read -rsp "Password admin (kosong = generate): " ADMIN_PASSWORD
    echo
  fi
  if [ -z "${ADMIN_PASSWORD:-}" ]; then
    ADMIN_PASSWORD="$(openssl rand -hex 12)"
    GENERATED_PASSWORD=1
  fi

  sed -e "s#/var/www/vodlocal#$APP_DIR#g" \
      -e "s#^VODLOCAL_DENSGO_BASE_URL=.*#VODLOCAL_DENSGO_BASE_URL=$DENSGO_BASE_URL#" \
      -e "/^VODLOCAL_SESSION_SECRET=/d" \
      -e "/^VODLOCAL_ADMIN_USER=/d" \
      -e "/^VODLOCAL_ADMIN_PASSWORD=/d" \
      -e "/^VODLOCAL_DRAMAHITS_BASE_URL=/d" \
      "$ROOT/deploy/vodlocal.env.example" > "$ENV_FILE"

  # Jaga-jaga bila contoh env tidak lagi memuat baris Densgo.
  if ! grep -q '^VODLOCAL_DENSGO_BASE_URL=' "$ENV_FILE"; then
    printf 'VODLOCAL_DENSGO_BASE_URL=%s\n' "$DENSGO_BASE_URL" >> "$ENV_FILE"
  fi

  {
    printf 'VODLOCAL_SESSION_SECRET=%s\n' "$SESSION_SECRET"
    printf 'VODLOCAL_ADMIN_USER=%s\n' "$ADMIN_USER"
    printf 'VODLOCAL_ADMIN_PASSWORD=%s\n' "$ADMIN_PASSWORD"
    printf 'VODLOCAL_DRAMAHITS_BASE_URL=%s\n' "$DRAMAHITS_BASE_URL"
  } >> "$ENV_FILE"
fi

chmod 600 "$ENV_FILE"
chown root:root "$ENV_FILE"

set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

: "${VODLOCAL_SESSION_SECRET:?VODLOCAL_SESSION_SECRET kosong di $ENV_FILE}"
: "${VODLOCAL_DRAMAHITS_BASE_URL:?VODLOCAL_DRAMAHITS_BASE_URL kosong di $ENV_FILE}"
[ "${#VODLOCAL_SESSION_SECRET}" -ge 16 ] || die "VODLOCAL_SESSION_SECRET minimal 16 karakter"
if [ -n "${VODLOCAL_ADMIN_USER:-}" ] && [ -z "${VODLOCAL_ADMIN_PASSWORD:-}" ]; then
  die "VODLOCAL_ADMIN_USER diisi tetapi VODLOCAL_ADMIN_PASSWORD kosong"
fi
if [ -z "${VODLOCAL_ADMIN_USER:-}" ] && [ -n "${VODLOCAL_ADMIN_PASSWORD:-}" ]; then
  die "VODLOCAL_ADMIN_PASSWORD diisi tetapi VODLOCAL_ADMIN_USER kosong"
fi

# 3. User dan permission
log "Menyiapkan user vodlocal"
getent group vodlocal >/dev/null || groupadd --system vodlocal
id -u vodlocal >/dev/null 2>&1 || useradd --system --gid vodlocal --home-dir "$APP_DIR" --shell /usr/sbin/nologin vodlocal

chown root:root "$BINARY"
chmod 755 "$APP_DIR" "$BINARY"
chown vodlocal:vodlocal "$APP_DIR/data"
chmod 755 "$APP_DIR/data"

# 4. Nginx dan port
if [ "${DISABLE_NGINX:-0}" = "1" ]; then
  log "Menonaktifkan nginx"
  systemctl disable --now nginx 2>/dev/null || true
fi

systemctl stop vodlocal 2>/dev/null || true

if [ "${SKIP_BACKUP:-0}" != "1" ] && [ -f "$APP_DIR/data/app.db" ]; then
  backup="$APP_DIR/data/app.db.bak-$(date +%Y%m%d-%H%M%S)"
  cp -a "$APP_DIR/data/app.db" "$backup"
  for suffix in -wal -shm; do
    if [ -f "$APP_DIR/data/app.db$suffix" ]; then
      cp -a "$APP_DIR/data/app.db$suffix" "$backup$suffix"
    fi
  done
  log "Backup database: $backup"
fi

APP_ADDR="${VODLOCAL_HTTP_ADDR:-:8080}"
APP_PORT="${APP_ADDR##*:}"
APP_PORT="${APP_PORT:-8080}"
APP_HOST="${APP_ADDR%:*}"
case "$APP_HOST" in
  ""|"0.0.0.0"|"[::]") APP_HOST="127.0.0.1" ;;
esac

if ss -ltn 2>/dev/null | awk -v p=":$APP_PORT" 'NR > 1 && $4 ~ p "$" { found = 1 } END { exit !found }'; then
  echo "Port $APP_PORT masih dipakai:"
  ss -ltnp 2>/dev/null | awk -v p=":$APP_PORT" 'NR == 1 || $4 ~ p "$"'
  if systemctl is-active --quiet nginx 2>/dev/null; then
    die "nginx masih aktif. Jalankan ulang dengan DISABLE_NGINX=1 atau hentikan nginx."
  fi
  die "hentikan proses di port $APP_PORT lalu jalankan ulang."
fi

if systemctl is-enabled --quiet nginx 2>/dev/null; then
  echo "PERINGATAN: nginx masih enabled; saat reboot bisa berebut port $APP_PORT."
  echo "           Gunakan DISABLE_NGINX=1 untuk menonaktifkannya."
fi

# 5. Systemd unit
log "Memasang systemd unit"
sed "s#/var/www/vodlocal#$APP_DIR#g" "$ROOT/deploy/vodlocal.service" > "$SERVICE_FILE"
chmod 644 "$SERVICE_FILE"
systemctl daemon-reload
systemctl enable vodlocal >/dev/null
if ! systemctl restart vodlocal; then
  echo "Service gagal start. Log terakhir:"
  journalctl -u vodlocal -n 80 --no-pager || true
  exit 1
fi

# 6. Health check
log "Menunggu service siap"
ok=0
for _ in $(seq 1 20); do
  if curl -fsS --max-time 2 "http://$APP_HOST:$APP_PORT/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 0.5
done

if [ "$ok" -ne 1 ]; then
  echo "Service belum sehat. Log terakhir:"
  journalctl -u vodlocal -n 80 --no-pager || true
  exit 1
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
IP="${IP:-127.0.0.1}"
if [ "$APP_PORT" = "80" ]; then
  BASE_URL="http://$IP"
else
  BASE_URL="http://$IP:$APP_PORT"
fi

log "VodLocal aktif"
echo "  Admin : $BASE_URL/login"
echo "  Health: $BASE_URL/health"
echo "  Log   : journalctl -u vodlocal -f"

if [ "$GENERATED_PASSWORD" = "1" ]; then
  echo
  echo "Kredensial admin dibuat:"
  echo "  User    : ${VODLOCAL_ADMIN_USER:-admin}"
  echo "  Password: $ADMIN_PASSWORD"
  echo "  Tersimpan di $ENV_FILE"
fi
