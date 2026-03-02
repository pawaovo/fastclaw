# FastClaw Docker Pool 发布说明（中文）

适用分支：`feature/docker-pool-runtime`  
推荐场景：单机部署（4C8G）  
推荐初始命令：`./init-reset.sh -3`

## 1. 本发布解决了什么

- 提供无 K8s 的单机运行模式（`runtime.mode = docker_pool`）。
- 提供 Portal 用户体系（邮箱注册/登录 + Google 登录可选）。
- 每用户固定 1 个专属 OpenClaw 实例（实例隔离，配置隔离）。
- 通过 `/proxy/{slug}/` 打开 OpenClaw 控制台。
- Portal 中支持 AI 配置和 Channel 配置持久化。
- 支持池满提示、健康守护、实例重建与回收。
- 提供一键部署/重置脚本，支持 `-1..-4` 池大小参数。

## 2. 与当前线上效果一致的行为

- 用户登录后可看到自己的实例卡片，并可 `Open / Start / Stop / Delete`。
- 实例内可创建多个 agent、配置多个 channel（Telegram/Discord/Feishu/WhatsApp 等）。
- 用户之间实例、配置、数据互相隔离，不共享 bot。
- Portal 中保存的 AI 配置会回显（包含 provider/baseUrl/modelId/apiType 等）。
- 资源池满时，新用户会收到明确提示（不会静默失败）。

## 3. 关键部署命令

首次部署（推荐）：

```bash
cd /opt
git clone -b feature/docker-pool-runtime https://github.com/pawaovo/fastclaw.git
cd /opt/fastclaw/deploy/docker-pool
chmod +x deploy.sh init-reset.sh rebuild-fastclaw-safe.sh
./init-reset.sh -3
```

日常更新：

```bash
cd /opt/fastclaw
git fetch pawaovo
git checkout feature/docker-pool-runtime
git pull --ff-only pawaovo feature/docker-pool-runtime
cd deploy/docker-pool
./deploy.sh -3
```

## 4. 发布后验收清单（建议逐项打勾）

1. `docker compose ps` 显示 `fastclaw-server / fastclaw-postgres / openclaw-1..N` 均为 `Up`。
2. `curl http://127.0.0.1:18080/health` 返回成功。
3. 打开 `http://<server-ip>:18080/portal` 能正常访问。
4. 新用户可注册并获得专属实例（或收到池满提示）。
5. 点击 `Open` 可进入控制台，状态可到“正常”。
6. 填写 AI 配置后可在聊天页成功返回模型回复。
7. Channel 配置可保存并在页面重新加载后回显。

## 5. 容量与限制

- 当前 compose 预置池实例上限为 `4`（`openclaw-1..4`）。
- 脚本参数 `-N` 当前支持 `1..4`。
- 4C8G 建议：
  - 起步 `-2` 或 `-3`
  - 观察 CPU/内存/磁盘后再考虑 `-4`

## 6. 重要配置项

- `deploy/docker-pool/config.toml`
  - `runtime.max_running_bots`
  - `docker_pool.endpoints`
  - `portal.google_*`
  - `domain.api_domain / bot_domain_template`
- `deploy/docker-pool/.env`
  - `POSTGRES_PASSWORD`（必须强密码）

## 7. 常见故障速查

1. Google 登录失败 `invalid_request`
- 回调地址和当前访问域名不一致。
- 检查 Google Console 的 redirect URI 与 `portal.google_redirect_url` 是否完全一致。

2. 控制台显示离线
- 先看 Portal `Health` 是否 `ready`。
- 检查 `openclaw-*` 与 `fastclaw-server` 最近日志。
- 重新进入页面并等待几秒让 websocket 建立。

3. 模型报无 API key
- 需要在 Portal `AI Config` 或 OpenClaw 内填写可用 provider/key/baseUrl/modelId。

## 8. 文档入口

- 详细部署手册：`docs/DEPLOY_DOCKER_POOL_ZH.md`
- docker-pool 目录说明：`deploy/docker-pool/README.md`

