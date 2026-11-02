#!/usr/bin/env bash
set -euo pipefail
umask 077

# Run inside the deployment directory after downloading compose.yaml and Caddyfile.
for file in compose.yaml Caddyfile; do
  [[ -f "$file" ]] || { echo "Missing $file in $PWD" >&2; exit 1; }
done
command -v openssl >/dev/null || { echo 'Install openssl first.' >&2; exit 1; }

mkdir -p data caddy/data caddy/config
chmod 700 data caddy
if [[ ! -e .env ]]; then
  JWT_VALUE="$(openssl rand -hex 32)"
  SETUP_VALUE="$(openssl rand -hex 24)"
  cat > .env <<EOF
SITE_DOMAIN=shop.aisale.best
APP_IMAGE=ghcr.io/axtoa/zovo_card_cdk_auto:latest
JWT_SECRET=${JWT_VALUE}
INSTALL_MODE=wizard
SETUP_BOOTSTRAP_TOKEN=${SETUP_VALUE}
UPDATE_ENABLED=0
EOF
  chmod 600 .env
  echo 'Created .env with random JWT and setup secrets.'
else
  echo 'Existing .env preserved.'
fi
echo 'Next: docker compose pull && docker compose up -d'
echo 'Setup: https://shop.aisale.best/ops/setup'
echo "Read your setup token locally: sed -n 's/^SETUP_BOOTSTRAP_TOKEN=//p' .env"
