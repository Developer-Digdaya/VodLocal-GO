#!/usr/bin/env bash
set -euo pipefail

# Development only. Produksi wajib mengisi variabel ini lewat systemd/env.
export VODLOCAL_DRAMAHITS_BASE_URL="${VODLOCAL_DRAMAHITS_BASE_URL:-https://stage-microdrama.digdaya.co.id/api/v1}"

# Admin awal untuk development. Hanya dipakai saat tabel auth_users kosong.
export VODLOCAL_ADMIN_USER="${VODLOCAL_ADMIN_USER:-admin}"
export VODLOCAL_ADMIN_PASSWORD="${VODLOCAL_ADMIN_PASSWORD:-admin12345}"

# Bind ke localhost agar password development tidak terbuka ke LAN.
export VODLOCAL_HTTP_ADDR="${VODLOCAL_HTTP_ADDR:-127.0.0.1:8080}"

cd "$(dirname "${BASH_SOURCE[0]}")/.."
exec go run ./cmd/server
