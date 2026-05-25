# 把 chatgpt2api 集成进 CPA-Manager 的实施方案

## 0. 目标

- 用户在服务器上从 3 个进程（CPA + CPA-Manager + chatgpt2api）变成 2 个（CPA + CPA-Manager-Plus）
- 一个面板入口，统一管理：CPA 账号、Usage 监控、图片号池、在线画图、图片 API
- 跟上游 `seakee/CPA-Manager` 和 `basketikun/chatgpt2api` 的版本同步路径都保持顺畅

## 1. 仓库与分支策略

```bash
# Fork seakee/CPA-Manager 到自己账号，命名比如 cpa-manager-plus
git clone git@github.com:<you>/cpa-manager-plus.git
cd cpa-manager-plus

# 跟踪上游
git remote add upstream https://github.com/seakee/CPA-Manager.git
# 可选：再跟踪上游的上游
git remote add upstream-root https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git

# 把 chatgpt2api 作为 submodule
git submodule add https://github.com/basketikun/chatgpt2api.git vendor/chatgpt2api

# 所有自有改动放 feature 分支，main 永远只跟 upstream/main 同步
git checkout -b feat/image-pool
```

**同步上游惯例：**
- `git fetch upstream && git merge upstream/main` 到 main
- `git rebase main` 到你的 feature 分支
- chatgpt2api 升级：`cd vendor/chatgpt2api && git fetch && git checkout <new-tag>` 然后回到根目录 commit submodule 指针

## 2. 镜像结构（关键文件）

新的 `Dockerfile.usage-service-plus`：

```dockerfile
# ---- stage 1: React 面板 ----
FROM node:22-alpine AS web-builder
WORKDIR /web
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build   # 产物：dist/index.html

# ---- stage 2: Go usage-service ----
FROM golang:1.23 AS go-builder
WORKDIR /src
COPY usage-service/ ./usage-service/
# 把 React 产物嵌入 usage-service（沿用上游 embed 机制）
COPY --from=web-builder /web/dist/index.html ./usage-service/internal/httpapi/web/management.html
WORKDIR /src/usage-service
RUN CGO_ENABLED=1 go build -o /out/cpa-manager ./cmd/cpa-manager

# ---- stage 3: chatgpt2api Python 依赖 ----
FROM python:3.12-slim AS py-builder
WORKDIR /app
COPY vendor/chatgpt2api/pyproject.toml vendor/chatgpt2api/uv.lock ./
RUN pip install --no-cache-dir uv && uv sync --frozen
COPY vendor/chatgpt2api/ ./

# ---- stage 4: runtime ----
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-pip ca-certificates \
      && rm -rf /var/lib/apt/lists/*

# s6-overlay 做进程管理
ARG S6_OVERLAY_VERSION=3.2.0.0
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz /tmp
RUN tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz \
 && tar -C / -Jxpf /tmp/s6-overlay-x86_64.tar.xz

COPY --from=go-builder /out/cpa-manager /usr/local/bin/cpa-manager
COPY --from=py-builder /app /opt/chatgpt2api
COPY --from=py-builder /app/.venv /opt/chatgpt2api/.venv
COPY docker/s6-rc.d /etc/s6-overlay/s6-rc.d

ENV S6_KEEP_ENV=1
ENV CHATGPT2API_BIND=127.0.0.1:8000

EXPOSE 18317
VOLUME ["/data"]
ENTRYPOINT ["/init"]
```

s6 服务定义（`docker/s6-rc.d/`）：
- `cpa-manager/run`：`/usr/local/bin/cpa-manager`
- `chatgpt2api/run`：`cd /opt/chatgpt2api && .venv/bin/python main.py`（监听 127.0.0.1:8000）

## 3. usage-service 改造

在 `usage-service/internal/httpapi/server.go` 的 `Handler()` 里追加一组路由：

```go
mux.HandleFunc("/v0/image/", s.withCORS(s.proxyChatGPT2API))      // 反代 chatgpt2api 内部 API
mux.HandleFunc("/v1/images/", s.withCORS(s.proxyChatGPT2APIOpenAI))// OpenAI 兼容透传
mux.HandleFunc("/v1/responses",  s.withCORS(s.proxyChatGPT2APIOpenAI))
mux.HandleFunc("/v1/chat/completions", s.withCORS(s.proxyChatGPT2APIOpenAI))
mux.HandleFunc("/v1/models",     s.withCORS(s.proxyChatGPT2APIOpenAI))
```

`proxyChatGPT2API` 实现要点：

1. 鉴权统一用 CPA 的 Management Key（从已有 `db.LoadManagerConfig` / `db.LoadSetup` 取），而不是 chatgpt2api 自己的 `auth-key`
2. 反代时把 `Authorization: Bearer <CHATGPT2API_INTERNAL_KEY>` 注入下游（内部 key 在容器启动时通过环境变量同步给 Python 进程）
3. 入参/出参不做 schema 改写，纯反代

## 4. chatgpt2api 侧的最小改动（在 fork 里 patch 而不是 push 到上游）

写一个 `vendor/chatgpt2api.patch` 或者在 Dockerfile 里 `COPY` 覆盖 `services/cpa_service.py` 的一段，把它"内嵌"模式切换为：

- 默认从环境变量 `CPA_BASE_URL` + `CPA_MANAGEMENT_KEY` 读 CPA 地址（不再要求用户在 chatgpt2api 自己的 UI 里二次配置）
- 启动时复用 CPA-Manager SQLite 里已经保存的 setup 信息（可以让 usage-service 把它 export 成环境变量）

这样 chatgpt2api 启动就**自动**指向同一个 CPA，不需要用户再填一次。

## 5. React 面板新增

在 `src/pages/` 新增一个 `ImagePool/` 目录，最小集合页面：

- **Image Account Pool**：表格 + 批量导入/刷新/删除（接 `/v0/image/accounts`）
- **Online Draw**：文生图 + 编辑图（接 `/v0/image/generations`）
- **Image API Settings**：查看/重置统一 auth key、查看 `/v1/models`

UI 可以直接参考 chatgpt2api 自己 `web/src/app/` 下的 `accounts/page.tsx` / `image/page.tsx` / `image-manager/page.tsx`，把样式改成 CPA-Manager 的 Tailwind/SCSS 风格。

在 `src/components/layout/MainLayout.tsx` 的侧边栏加入口。

## 6. docker-compose 示例（用户最终使用）

```yaml
services:
  cli-proxy-api:
    image: ghcr.io/router-for-me/cli-proxy-api:latest
    restart: unless-stopped
    ports: ["8317:8317"]
    volumes: [cpa-data:/data]

  cpa-manager-plus:
    image: <you>/cpa-manager-plus:latest
    restart: unless-stopped
    ports: ["18317:18317"]
    volumes: [cpa-manager-data:/data]
    depends_on: [cli-proxy-api]

volumes:
  cpa-data:
  cpa-manager-data:
```

用户只需要打开 `http://host:18317/management.html`：
- 输入 CPA URL + Management Key（沿用现有流程）
- 在新加的 "Image Pool" tab 里点 "Sync from CPA"，号池自动从 CPA auth files 拉取

## 7. 风险与待办

| 风险 | 缓解 |
|---|---|
| chatgpt2api 依赖 `curl_cffi`（带 chromium TLS 指纹）镜像体积变大 | 接受 ~150MB 额外体积；或者用 distroless+静态化（复杂） |
| ChatGPT 反爬变更（PoW/Turnstile） | 锁 chatgpt2api submodule 到稳定 tag，每周/双周升级一次 |
| 上游 seakee/CPA-Manager 改动 React 路由 | feature 分支只增不改文件，rebase 冲突最小化 |
| usage-service 端口 18317 暴露图片 API 后流量变大 | 给 `/v1/*` 加独立 rate limit + 日志 |
| CPA-Manager 的 React 是单文件 HTML，要保证嵌入不破坏现有 SPA | 单页 SPA + react-router，新页面只是新路由，不影响 |

## 8. 实施顺序（最小可用 → 完善）

1. **Day 1-2**：fork、加 submodule、写 Dockerfile + s6，本地把容器跑起来，浏览器能同时访问 18317 面板和（容器内的）chatgpt2api API
2. **Day 3**：usage-service 加 `/v1/*` 反代和鉴权统一
3. **Day 4-5**：React 加 ImagePool 页面（先做账号池 + 一键从 CPA 同步）
4. **Day 6**：React 加在线画图页（chatgpt2api 现有 UI 移植）
5. **Day 7**：写 README、docker-compose 示例，发自己 DockerHub
6. **持续**：每次上游升级走 `git fetch upstream && git merge` + `git submodule update --remote`

## 9. 如果想极简

不想动 s6 / 多进程，可以把 chatgpt2api 用 `docker-compose` 跟 cpa-manager 放一个 compose file，network 共享，让 usage-service 反代到 `http://chatgpt2api:8000`。**用户视角仍然是一个 docker compose 命令，一个面板**，缺点是镜像数 = 2。这是 Plan A 的"轻量版"，5 分钟可以跑起来。

---

## 10. 实施回顾（2026-05-25 更新）

### Phase 1-2.5 按原计划落地
- Phase 1：多进程镜像 + s6-overlay 跑通
- Phase 2：Go 反代 + 内部 auth-key 桥接 + chatgpt2api 的 cpa_service.py.patch
- Phase 2.5：`/v1/images/*` 智能路由 + dormant CPA fallback 骨架
- 实测：成功通过统一面板路径 (POST /v1/images/generations) 端到端生图

### 跟 §2 / §7 计划的偏差：抽取 chatgpt2api 核心进项目，删除 submodule

**原计划**（§7 风险表 + §8 步骤 6）：用 `git submodule update --remote` 跟 chatgpt2api
上游。带来两个真实问题：
1. **账号池数据双份**：CPA 持有 OAuth 文件，chatgpt2api 又维护自己的 accounts.json，
   CPA 后台 silent refresh access_token 时本地副本就过期，要靠定时 sync 补救。
2. **维护责任错配**：chatgpt2api 内部的 PoW/Turnstile/TLS 指纹由上游维护没问题，但
   "用户调生图 → 自动用 CPA 池里的 free 号"这一层语义是我们项目独有的，submodule
   做不了。

**新架构**（spike/extract-image-core 验证了不要全重写之后选的路 B）：
- 删 `vendor/chatgpt2api` submodule + `.gitmodules`
- 新增 `image-service/` 顶层目录，进项目跟踪
- 复用 chatgpt2api 的 ChatGPT 反爬核心代码（`openai_backend_api.py` / `conversation.py`
  / `pow.py` / `turnstile.py` / `helper.py`，约 2,400 LOC 原样保留）
- **重写 `account_service.py`**：CPA 是唯一真相源，按 modtime 缓存文件列表，
  按需 download token，401 时 invalidate 并 redownload，覆盖 CPA 续期场景
- 砍掉 chatgpt2api 自带的 web UI / backup / register / chat-completions / 多后端
  存储 / multi-user auth（约 5,000 LOC 不再维护）
- s6 service `chatgpt2api` 重命名为 `image-service`，监听同样的 127.0.0.1:8000
- 镜像体积从 124MB → 80MB（删了 sqlalchemy / psycopg2 / gitpython 等）

**对外行为零变化**：
- 面板路由（`/v1/images/generations`、`/v1/images/edits`）的 URL、参数、返回结构都不变
- Phase 2 的反代 + auth bridge 不动
- Phase 2.5 的 smart router + CPA fallback 不动

**维护语义变化**：
- 之前："锁 chatgpt2api submodule 到稳定 tag，每周/双周升级一次"
- 现在："image-service 是我们的代码。ChatGPT 协议变化需要手动跟上游 chatgpt2api 项目 diff，
  重要 fix 手动 cherry-pick 进来。" 上游 PoW/Turnstile 更新仍然是绕不开的责任，
  只是不靠 submodule 自动拉，而是定期看上游 commit。

**测试**：
- 9 个 account_service 单元测试（mock CPA HTTP）
- 容器内端到端真生图测试：HTTP 200 + 2.5MB PNG 在 70 秒内返回，使用一个 ChatGPT free
  账号的 access_token（OAuth），CPA 池里的 121 个号被 image-service 0 同步直接读取。

