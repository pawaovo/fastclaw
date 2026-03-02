#!/usr/bin/env sh
set -eu

# Safe rebuild flow for low-memory hosts:
# 1) stop OpenClaw pool containers
# 2) rebuild/recreate fastclaw service
# 3) start OpenClaw pool containers back

POOL_CONTAINERS="${POOL_CONTAINERS:-openclaw-1 openclaw-2 openclaw-3 openclaw-4}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"

echo "[safe-rebuild] stopping pool containers: ${POOL_CONTAINERS}"
for c in ${POOL_CONTAINERS}; do
  docker stop "${c}" >/dev/null 2>&1 || true
done

echo "[safe-rebuild] rebuilding fastclaw service"
docker compose -f "${COMPOSE_FILE}" up -d --build --no-deps fastclaw

echo "[safe-rebuild] starting pool containers back"
for c in ${POOL_CONTAINERS}; do
  docker start "${c}" >/dev/null 2>&1 || true
done

echo "[safe-rebuild] done"
docker compose -f "${COMPOSE_FILE}" ps fastclaw
