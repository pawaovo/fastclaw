#!/usr/bin/env sh
set -eu

# One-click deploy for docker_pool mode.
# Supports:
#   ./deploy.sh -3
#   ./deploy.sh --instances 3
#   ./deploy.sh --instances 3 --init

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
cd "$SCRIPT_DIR"

POOL_SIZE=4
DO_INIT=0

usage() {
  cat <<'EOF'
Usage:
  ./deploy.sh -N
  ./deploy.sh --instances N [--init]

Options:
  -N, --instances N   openclaw pool size (1-4)
  --init              reset DB data (users/bots/allocations/apps) and clear openclaw runtime data
  -h, --help          show help
EOF
}

is_number() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$1"
    return
  fi
  # Fallback for minimal systems.
  dd if=/dev/urandom bs=1 count="$1" 2>/dev/null | od -An -tx1 | tr -d ' \n'
}

set_or_replace_kv() {
  key="$1"
  value="$2"
  file="$3"
  if grep -q "^${key}[[:space:]]*=" "$file"; then
    sed -i -E "s|^${key}[[:space:]]*=.*|${key} = ${value}|" "$file"
  else
    printf '%s = %s\n' "$key" "$value" >> "$file"
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --init)
      DO_INIT=1
      shift
      ;;
    --instances)
      shift
      [ "$#" -gt 0 ] || { echo "missing value for --instances" >&2; exit 1; }
      POOL_SIZE="$1"
      shift
      ;;
    -[0-9]*)
      POOL_SIZE="${1#-}"
      shift
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if ! is_number "$POOL_SIZE"; then
  echo "instances must be a number, got: $POOL_SIZE" >&2
  exit 1
fi
if [ "$POOL_SIZE" -lt 1 ] || [ "$POOL_SIZE" -gt 4 ]; then
  echo "instances must be in range 1-4, got: $POOL_SIZE" >&2
  exit 1
fi

if [ ! -f ".env" ]; then
  cp .env.example .env
fi
if ! grep -q '^POSTGRES_PASSWORD=' .env; then
  printf 'POSTGRES_PASSWORD=%s\n' "$(random_hex 24)" >> .env
fi

POSTGRES_PASSWORD="$(grep '^POSTGRES_PASSWORD=' .env | tail -n1 | cut -d= -f2-)"
if [ -z "$POSTGRES_PASSWORD" ]; then
  echo "POSTGRES_PASSWORD is empty in .env" >&2
  exit 1
fi

if [ ! -f "config.toml" ]; then
  cp config.toml.example config.toml
fi

# Keep db password in config consistent with .env.
escaped_db_pass=$(printf '%s' "$POSTGRES_PASSWORD" | sed 's/"/\\"/g')
set_or_replace_kv "password" "\"${escaped_db_pass}\"" "config.toml"
set_or_replace_kv "max_running_bots" "$POOL_SIZE" "config.toml"

endpoints=""
names=""
i=1
while [ "$i" -le "$POOL_SIZE" ]; do
  if [ -n "$endpoints" ]; then
    endpoints="$endpoints, "
    names="$names, "
  fi
  endpoints="${endpoints}\"openclaw-${i}:18789\""
  names="${names}\"openclaw-${i}\""
  i=$((i + 1))
done
sed -i -E "s|^endpoints = .*|endpoints = [${endpoints}]|" config.toml
sed -i -E "s|^container_names = .*|container_names = [${names}]|" config.toml

# Ensure secrets are not left as defaults in fresh deployments.
if grep -q '^admin_token = "change-me-admin-token"' config.toml; then
  set_or_replace_kv "admin_token" "\"$(random_hex 20)\"" "config.toml"
fi
if grep -q '^session_secret = "change-this-portal-session-secret"' config.toml; then
  set_or_replace_kv "session_secret" "\"$(random_hex 24)\"" "config.toml"
fi

echo "[deploy] pool_size=$POOL_SIZE init=$DO_INIT"
echo "[deploy] starting postgres..."
docker compose up -d postgres

if [ "$DO_INIT" -eq 1 ]; then
  echo "[deploy] resetting database schema..."
  docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" fastclaw-postgres \
    psql -U postgres -d fastclaw -v ON_ERROR_STOP=1 \
    -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;"
fi

# Start selected openclaw services.
SERVICES=""
i=1
while [ "$i" -le "$POOL_SIZE" ]; do
  SERVICES="$SERVICES openclaw-$i"
  i=$((i + 1))
done

echo "[deploy] starting openclaw services:$SERVICES"
docker compose up -d $SERVICES

if [ "$DO_INIT" -eq 1 ]; then
  echo "[deploy] clearing openclaw runtime data..."
  i=1
  while [ "$i" -le "$POOL_SIZE" ]; do
    docker exec "openclaw-$i" sh -lc 'rm -rf /home/node/.openclaw/* && mkdir -p /home/node/.openclaw && chown -R node:node /home/node/.openclaw || true'
    i=$((i + 1))
  done
fi

# Stop extra pool services.
i=$((POOL_SIZE + 1))
while [ "$i" -le 4 ]; do
  docker compose stop "openclaw-$i" >/dev/null 2>&1 || true
  i=$((i + 1))
done

echo "[deploy] rebuilding fastclaw..."
if [ -x "./rebuild-fastclaw-safe.sh" ]; then
  ./rebuild-fastclaw-safe.sh
else
  docker compose up -d --build fastclaw
fi

# Ensure extra services remain stopped (safe script may have started all).
i=$((POOL_SIZE + 1))
while [ "$i" -le 4 ]; do
  docker compose stop "openclaw-$i" >/dev/null 2>&1 || true
  i=$((i + 1))
done

echo "[deploy] waiting for health..."
tries=0
until curl -fsS http://127.0.0.1:18080/health >/dev/null 2>&1; do
  tries=$((tries + 1))
  if [ "$tries" -ge 30 ]; then
    echo "[deploy] fastclaw health check timed out" >&2
    docker compose ps
    exit 1
  fi
  sleep 1
done

echo "[deploy] done"
docker compose ps
echo "[deploy] portal: http://<server-ip>:18080/portal"
