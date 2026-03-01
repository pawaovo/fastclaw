# FastClaw Docker Pool Deployment (Single Server)

This deployment is for a single 4C8G server using `runtime.mode = "docker_pool"`.

## 1) Prepare config

```bash
cd deploy/docker-pool
cp config.toml.example config.toml
```

Edit `config.toml`:
- `api.admin_token`
- `db.password`
- `domain.api_domain` and `domain.bot_domain_template` to your actual public URL
- `portal.session_secret`
- `portal.google_client_id`
- `portal.google_client_secret`
- `portal.google_redirect_url` (must match Google Console exactly)

## 2) Start services

```bash
cd deploy/docker-pool
docker compose up -d --build
```

Services started:
- `fastclaw-server` on `:18080`
- `fastclaw-postgres`
- `openclaw-1..4` (resource-capped)

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

- `max_running_bots = 2` is recommended for 4C8G to avoid OOM/CPU exhaustion.
- `docker_pool.endpoints` defines your pre-provisioned OpenClaw gateway instances.
- In `docker_pool` mode, K8s-only APIs are intentionally blocked.
- End-user portal is available at `/portal` after setting Google OAuth config.
