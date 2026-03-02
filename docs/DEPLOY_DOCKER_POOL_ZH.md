# FastClaw 单机 Docker Pool 部署指南（中文）

本文档对应当前分支 `feature/docker-pool-runtime`，目标是复现你现在线上可用的效果：

- 用户通过 `Portal` 注册/登录（支持 Google OAuth）。
- 每个用户固定最多 1 个专属 OpenClaw 实例（实例内可配多个 Channel/Agent）。
- 通过 `proxy/{slug}` 打开 OpenClaw 控制台直接使用。
- 支持在 Portal 中保存 AI 配置与 Channel 配置并持久化。
- 适配 4C8G 单机，资源池建议起步 3 个实例（`-3`）。

## 1. 服务器要求

- Ubuntu / Debian Linux（已安装 Docker + Docker Compose 插件）
- 建议最低：`4C8G`
- 放行端口：
  - `18080`（FastClaw Portal/API/Proxy）
  - `12576`（如果你用 1Panel）

## 2. 拉取代码

```bash
cd /opt
git clone -b feature/docker-pool-runtime https://github.com/pawaovo/fastclaw.git
cd /opt/fastclaw/deploy/docker-pool
```

## 3. 一键初始化并部署（推荐）

首次部署直接执行：

```bash
chmod +x deploy.sh init-reset.sh rebuild-fastclaw-safe.sh
./init-reset.sh -3
```

说明：

- `-3` 表示启动 `openclaw-1..3` 三个实例池（支持范围 `1-4`）。
- `init-reset` 会清空数据库业务表并清理池实例运行态数据，适合首次上线或重置环境。
- 脚本会自动生成/更新：
  - `.env` 的 `POSTGRES_PASSWORD`
  - `config.toml` 的 `db.password`
  - `runtime.max_running_bots`
  - `docker_pool.endpoints` 与 `container_names`

非重置发布可用：

```bash
./deploy.sh -3
```

## 4. 验证服务状态

```bash
docker compose ps
curl -s http://127.0.0.1:18080/health
```

预期：

- `fastclaw-server` 为 `Up`
- `fastclaw-postgres` 为 `Up`
- `openclaw-1..3` 为 `Up`
- `/health` 返回 `ok`

## 5. 首次访问与用户流程

访问：

- `http://<你的服务器IP>:18080/portal`

用户逻辑：

1. 用户注册/登录（本地邮箱或 Google）。
2. 系统尝试自动分配专属实例。
3. 用户在“我的 OpenClaw 实例”点击 `Open` 进入控制台。
4. 用户在 Portal 的 `AI Config` 或 OpenClaw 内完成模型配置后开始对话。

配额规则：

- 每用户固定上限 1 实例。
- 池满时新用户会收到“资源池已满”提示，已有用户不受影响。

## 6. Google OAuth 配置（可选但推荐）

你需要在 `Google Cloud Console -> OAuth Client` 设置：

- `Authorized JavaScript origins`:
  - `http://<服务器IP>:18080`
  - 或你的公网域名（推荐）
- `Authorized redirect URIs`:
  - `http://<服务器IP>:18080/portal/auth/google/callback`
  - 若走隧道/域名，则必须与实际访问域名完全一致

并在 `deploy/docker-pool/config.toml` 中设置：

- `portal.google_client_id`
- `portal.google_client_secret`
- `portal.google_redirect_url`

修改后重启：

```bash
cd /opt/fastclaw/deploy/docker-pool
./deploy.sh -3
```

## 7. AI 配置建议

本项目已支持在 Portal 中直接保存 AI 参数（持久化回显）：

- `provider`
- `baseUrl`
- `apiKey`
- `modelId`
- `modelName`
- `apiType`
- `auth`
- `maxTokens`
- `contextWindow`

推荐让用户自行填写自己的服务商参数，系统默认不预置有效密钥。

## 8. Channel（Telegram/Discord/Feishu/WhatsApp）说明

- 可在 Portal 的 `Channels` 面板中配置各平台参数并持久化。
- Telegram 在 `pairing` 模式下，首次对话后需配对批准（已提供 Portal 按钮与后端 API 支撑）。
- 每个用户的配置写入其专属实例，彼此隔离。

## 9. 升级发布流程（生产建议）

```bash
cd /opt/fastclaw
git fetch pawaovo
git checkout feature/docker-pool-runtime
git pull --ff-only pawaovo feature/docker-pool-runtime
cd deploy/docker-pool
./deploy.sh -3
```

低内存机器建议使用安全重建：

```bash
./rebuild-fastclaw-safe.sh
```

## 10. 常见问题排查

1. OpenClaw 控制台显示离线
- 先看 Portal 中实例 `Health` 是否 `ready`。
- 再看容器日志：
  - `docker logs --since 10m openclaw-1`
  - `docker logs --since 10m fastclaw-server`
- 本分支已修复 docker_pool 下代理 query token 透传问题。

2. Google 登录 `invalid_request`
- 回调 URL 与当前访问域名不一致。
- 必须将实际访问地址写入 Google OAuth 配置。

3. 新用户无法分配实例
- 资源池已满（`max_running_bots` 或池实例被占满）。
- 可扩到 `-4`，或释放/删除闲置实例。

4. 机器卡顿/磁盘高占用
- 4C8G 建议从 `-2` 或 `-3` 起步。
- 定期清理：
  - `docker system df`
  - `docker image prune -f`
  - `docker builder prune -f --filter until=240h`

## 11. 对外发布建议

建议将以下内容写进你的仓库 README 首页：

- 推荐部署命令：`./init-reset.sh -3`
- 访问入口：`http://<ip>:18080/portal`
- 配额策略：每用户 1 实例
- 池大小可配：`-1..-4`
- Google OAuth 配置要点与回调地址要求

