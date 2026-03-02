# FastClaw Docker Pool Deployment (Single Server)

This deployment is for a single 4C8G server using `runtime.mode = "docker_pool"`.

## Quick start (recommended)

```bash
cd deploy/docker-pool
chmod +x deploy.sh init-reset.sh rebuild-fastclaw-safe.sh
./init-reset.sh -3
```

After it finishes:

- Open `http://<server-ip>:18080/portal`
- Register/Login (local email or Google OAuth)
- Open your dedicated instance and configure AI provider in `AI Config`

Chinese full guide:

- [`docs/DEPLOY_DOCKER_POOL_ZH.md`](../../docs/DEPLOY_DOCKER_POOL_ZH.md)

## 1) Prepare config

```bash
cd deploy/docker-pool
cp config.toml.example config.toml
cp .env.example .env
```

Edit `config.toml`:
- `api.admin_token`
- `db.password`
- `domain.api_domain` and `domain.bot_domain_template` to your actual public URL
- `portal.session_secret`
- `portal.google_client_id`
- `portal.google_client_secret`
- `portal.google_redirect_url` (must match Google Console exactly)

Edit `.env`:
- `POSTGRES_PASSWORD` (must be a strong random password, do not use `change-me`)

## 2) Start services

```bash
cd deploy/docker-pool
docker compose up -d --build
```

Services started:
- `fastclaw-server` on `:18080`
- `fastclaw-postgres`
- `openclaw-1..4` (resource-capped)

## One-click deploy (recommended)

Use the built-in script to deploy and set pool size quickly:

```bash
cd deploy/docker-pool
chmod +x deploy.sh init-reset.sh rebuild-fastclaw-safe.sh
./deploy.sh -3
```

Notes:
- `-3` means create/start up to 3 OpenClaw pool instances (`openclaw-1..3`).
- Supported range in current compose: `1-4`.
- The script auto-generates/updates:
  - `.env` (`POSTGRES_PASSWORD`)
  - `config.toml` (`db.password`, `max_running_bots`, `docker_pool.endpoints`, `container_names`)
- It then starts postgres, selected openclaw instances, rebuilds fastclaw, and verifies `/health`.

### Full initialization reset + deploy

If you want to clear users/bots/allocations/apps and reset runtime state:

```bash
cd deploy/docker-pool
./init-reset.sh -3
```

This will:
- Reset `fastclaw` database schema (all portal/app/bot/runtime records removed)
- Clear `/home/node/.openclaw` runtime data in started pool containers
- Redeploy with the requested pool size

## 2.1) Keep server-local overrides out of git

Use `docker-compose.override.yml` for machine-specific tweaks (ports, limits, bind mounts, etc.).

```bash
cd deploy/docker-pool
cp docker-compose.override.example.yml docker-compose.override.yml
# edit docker-compose.override.yml for this server only
docker compose up -d --build
```

Do not edit `docker-compose.yml` directly on production hosts. Keep it tracking upstream so `git pull` stays conflict-free.

## 3) Verify

```bash
curl http://127.0.0.1:18080/health

docker compose ps
docker compose logs -f fastclaw
```

## 4) Create first app + bot

```bash
ADMIN_TOKEN="<your-admin-token>"
BASE="http://127.0.0.1:18080"

APP_RESP=$(curl -s -X POST "$BASE/bot/api/v1/admin/apps" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"demo-app"}')

echo "$APP_RESP"
API_TOKEN=$(echo "$APP_RESP" | sed -n 's/.*"api_token":"\([^"]*\)".*/\1/p')

BOT_RESP=$(curl -s -X POST "$BASE/bot/api/v1/bots" \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"user_id":"u1","name":"bot-u1","slug":"bot-u1"}')

echo "$BOT_RESP"
BOT_ID=$(echo "$BOT_RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

curl -s -X POST "$BASE/bot/api/v1/bots/$BOT_ID/start" \
  -H "Authorization: Bearer $API_TOKEN"

curl -s "$BASE/bot/api/v1/bots/$BOT_ID/status" \
  -H "Authorization: Bearer $API_TOKEN"
```

## Notes

- For 4C8G, start with `max_running_bots = 2`, then raise gradually (for example to `4`) only after observing stable memory/CPU usage.
- `docker_pool.endpoints` defines your pre-provisioned OpenClaw gateway instances.
- In `docker_pool` mode, K8s-only APIs are intentionally blocked.
- End-user portal is available at `/portal` after setting Google OAuth config.

## Update workflow

Use this safe sequence when syncing code updates on a production server:

```bash
cd /opt/fastclaw
git fetch --all
git pull
cd deploy/docker-pool
docker compose up -d --build fastclaw
```

If you already changed tracked files locally, stash first, then re-apply only necessary settings into `docker-compose.override.yml`.

### Low-memory server rebuild (recommended for 4C8G)

On 4C8G hosts, a direct `docker compose up -d --build fastclaw` can be OOM-killed while compiling.
Use the helper script below to temporarily stop pool containers, rebuild `fastclaw`, then restore pool containers automatically:

```bash
cd deploy/docker-pool
chmod +x rebuild-fastclaw-safe.sh
./rebuild-fastclaw-safe.sh
```

### Disk cleanup (recommended before/after upgrades)

```bash
docker system df
docker image prune -f
docker builder prune -f --filter until=240h
docker container prune -f
```
