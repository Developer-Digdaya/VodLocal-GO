#!/usr/bin/env bash
set -euo pipefail

# Development only. Produksi wajib mengisi variabel ini lewat systemd/env.
export VODLOCAL_DRAMAHITS_BASE_URL="${VODLOCAL_DRAMAHITS_BASE_URL:-https://stage-microdrama.digdaya.co.id/api/v1}"

# Base API Densgo (tab Movie & Series). Nilai env yang sudah ada tetap dipakai.
export VODLOCAL_DENSGO_BASE_URL="${VODLOCAL_DENSGO_BASE_URL:-https://pp.dens.tv:777/index.php/v3070/adps_list_movies_v1}"

# Admin awal untuk development. Hanya dipakai saat tabel auth_users kosong.
export VODLOCAL_ADMIN_USER="${VODLOCAL_ADMIN_USER:-admin}"
export VODLOCAL_ADMIN_PASSWORD="${VODLOCAL_ADMIN_PASSWORD:-admin12345}"

# Bind ke localhost agar password development tidak terbuka ke LAN.
export VODLOCAL_HTTP_ADDR="${VODLOCAL_HTTP_ADDR:-127.0.0.1:8080}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."
exec go run ./cmd/server
