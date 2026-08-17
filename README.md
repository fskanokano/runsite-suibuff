# runsite-suibuff

[freebuff-proxy](https://github.com/trefeon/freebuff-proxy)（Go 编写的 OpenAI 兼容网关，把 Codebuff/FreeBuff CLI 协议转成 OpenAI API）的 **Runsite 适配版**。

上游源码**零改动**，所有适配都在构建文件和文档里。连接 GitHub 仓库 push 即部署，Runsite 免费档免绑卡、web service 永久免费、活跃应用无冷启动。

- **上游版本**: [`trefeon/freebuff-proxy`](https://github.com/trefeon/freebuff-proxy) @ `77bf41e` (2026-08-17)
- **平台**: [Runsite](https://runsite.app) 免费档 — 1× web service（0.1 vCPU / 256MB / 100GB 带宽），免卡，EU·法兰克福

---

## 改动点（相对上游）

| 文件 | 类型 | 说明 |
|---|---|---|
| `Dockerfile` | 修改（+4 行 ENV） | 上游多阶段构建原样保留；新增环境变量预设：`LISTEN_ADDR=:8080`（Runsite 默认端口）、`AUTO_DISCOVER_TOKEN=false`（云上必设）、`SAFE_MODE=true`、`COST_MODE=free`；`EXPOSE` 3457 → 8080 |
| `.env.runsite` | 新增 | 可选环境变量清单（必设项已内置在 Dockerfile，无需手动填） |
| `README.md`（本文件） | 替换 | 部署指南；上游 README 全文见 [trefeon/freebuff-proxy](https://github.com/trefeon/freebuff-proxy) |
| `.gitignore` | 微调 | 放行 `!.env.runsite`（上游规则会忽略所有 `.env.*`） |
| `cmd/` `internal/` `go.mod` 等 | 原样 | 上游代码，零改动 |

**为什么这两行是关键**：
- `LISTEN_ADDR=:8080` — 上游默认 `127.0.0.1:3457` 只监听回环，容器外（Runsite 平台）访问不到；Runsite 默认把流量转发到容器 **8080** 端口
- `AUTO_DISCOVER_TOKEN=false` — env-only 变量，云上无 `~/.config/codebuff` 登录文件，必须关闭避免 bridge 模式意外翻成空的 pooled 模式

> 换端口：改环境变量 `LISTEN_ADDR`（如 `:3457`），并在 Runsite 的 **Settings → Build & Deploy** 里把端口改成相同值。

---

## 部署（Runsite 控制台）

1. 注册 [Runsite](https://runsite.app)（免卡，GitHub 登录即可）
2. Dashboard → **New Web Service** → **Deploy from Git** → 连接 `fskanokano/runsite-suibuff` 仓库（GitHub / GitLab / Bitbucket 都支持）
3. Runsite 检测到仓库根目录有 `Dockerfile`，会直接用 Dockerfile 构建（自动检测 Go 也可，但 Dockerfile 更可控）
4. **Settings → Health checks**：健康检查路径填 `/healthz`（freebuff-proxy 的免鉴权健康端点；默认路径探测可能导致误判）
5. （可选）**Settings → Environment**：需要的话填 `AUTH_TOKENS` / `ADMIN_TOKEN` / `API_KEYS`（见 [.env.runsite](.env.runsite)）
6. 点 **Deploy**，约 30 秒构建完成，拿到 `https://<service>.runsite.app` 域名
7. 之后每次 `git push origin main` 自动重新构建部署（零停机滚动更新）

---

## 验证

```bash
# 健康检查（免鉴权）
curl -s https://<your-service>.runsite.app/healthz
# → {"status":"ok",...}

# 模型列表
curl -s https://<your-service>.runsite.app/v1/models
# → 200，模型列表

# OpenAI 兼容对话（bridge 模式：客户端自带上游 token）
curl -s https://<your-service>.runsite.app/v1/chat/completions \
  -H "Authorization: Bearer <client-key-or-upstream-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"...","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

---

## 云上差异（对比本机运行）

- **无冷启动**：活跃应用保持 warm；只有连续 **14 天**无请求才休眠（对比 Render 的 15 分钟），会话/run 池基本常驻
- **SSE 流式 / WebSocket**：容器原生透传，无缓冲无超时（官方明确支持 WebSocket）
- **uTLS stealth**：原生 Linux 容器，TLS 指纹伪装完整可用
- **自动部署**：push main 自动构建 + 零停机滚动更新 + 健康检查失败自动回滚
- **出口**：EU·法兰克福（德国）。⚠️ 不是美国出口——如强依赖美国出口（如访问美国上游服务）需评估延迟/可达性
- **会话持久化**：容器重启丢内存态会话（上游支持 `SESSION_PERSIST` 落盘可选启用；免费档无持久盘，重启即重置）

## 免费档边界（Runsite Free）

- 1 个 web service：0.1 vCPU / 256MB RAM / 1GB 磁盘 / 100GB 带宽，**永久免费，免绑卡**
- 免费数据库（Postgres/Redis）只免费 **30 天**，之后需升级——本项目不需要数据库，无影响
- 14 天无任何请求的应用会被休眠（下次访问冷启动唤醒）
- Public Beta 阶段，平台可能有变动
