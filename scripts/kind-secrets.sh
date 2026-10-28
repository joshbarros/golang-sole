#!/usr/bin/env bash
# Writes deploy/k8s/overlays/kind/secrets.env once: demo credentials for the local cluster. The signing key is random; the
# administrator is the one the browser specs and the smoke script expect. Nothing here is for a real deployment.
set -euo pipefail
file="$(dirname "$0")/../deploy/k8s/overlays/kind/secrets.env"
[ -f "$file" ] && exit 0
rand() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c "$1"; }
cat >"$file" <<ENV
POSTGRES_PASSWORD=$(rand 24)
RABBITMQ_PASSWORD=platform
JWT_KEY=$(rand 48)
ADMIN_EMAIL=admin@sole.local
ADMIN_PASSWORD=${ADMIN_PASSWORD:-SoleAdmin2026}
ENV
echo "wrote $file"
