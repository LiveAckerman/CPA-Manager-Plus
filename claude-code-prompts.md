# Claude Code 分阶段 Prompt 集合

> 使用说明：每个 Phase 在**独立的 Claude Code 会话**里跑（不要一次粘贴全部）。每个 Phase 都先确认上一阶段的验收 checklist 全过，再开下一个。如果 Claude Code 卡住或开始乱猜，复制下面的"刹车指令"打断它：
>
> > 停。回到 prompt 里的 "Ask before doing" 列表，按列表逐条问我，等我回答再继续。

---

## 通用上下文（每个 Phase 开头都要带）

```
项目背景：
我在 fork seakee/CPA-Manager（一个 CLIProxyAPI 的 React 管理面板 + Go usage-service），
目标是把 basketikun/chatgpt2api（一个 Python FastAPI 服务，用 ChatGPT 网页版 access_token
跑 gpt-image-2）的功能集成进来，让用户只用一个 docker 容器、一个面板、一个端口就够。

完整方案在 integration-plan.md，已附在仓库根目录。请先读这份文档。

技术栈：
- 前端：React 19 + Vite + Zustand + Tailwind/SCSS，打包成单文件 HTML
- usage-service：Go 1.23，net/http，SQLite (mattn/go-sqlite3)
- chatgpt2api：Python 3.12 + FastAPI + uvicorn + curl_cffi + PIL

工作规则：
1. 永远只在 feature 分支改动，不污染 main（main 跟踪 upstream/seakee）
2. 不修改 vendor/chatgpt2api 下的文件——所有 patch 通过 Dockerfile COPY 覆盖
3. 遇到下面 "Ask before doing" 列表里的任一项，先停下来问我
4. 完成后跑一遍 "Acceptance" 里的所有检查，把结果贴给我
```

---

## Phase 1：仓库骨架 + 多进程镜像（Day 1-2）

```
【Phase 1 / 5】仓库骨架与 Docker 镜像

任务范围（只做这些）：
1. 在仓库根目录新建 vendor/chatgpt2api 作为 git submodule，
   指向 https://github.com/basketikun/chatgpt2api.git 的最新 tag（不是 main）
2. 新建 Dockerfile.usage-service-plus，多阶段构建：
   a. stage web-builder: node:22-alpine, npm ci + npm run build，产物 dist/index.html
   b. stage go-builder: golang:1.23, 把 web 产物 embed 进 usage-service，构建 cpa-manager 二进制
   c. stage py-builder: python:3.12-slim, 用 uv 安装 vendor/chatgpt2api 依赖到 .venv
   d. stage runtime: debian:bookworm-slim + python3 + s6-overlay v3.2.0.0
3. 新建 docker/s6-rc.d/ 目录，包含两个 longrun 服务：
   a. cpa-manager: 运行 /usr/local/bin/cpa-manager，监听 0.0.0.0:18317
   b. chatgpt2api: 运行 /opt/chatgpt2api/.venv/bin/python /opt/chatgpt2api/main.py，
      通过环境变量 CHATGPT2API_BIND=127.0.0.1:8000 让它只监听 localhost
   c. 用户级别（user/contents.d）启用两个服务
4. 新建 docker-compose.dev.yml 用于本地开发测试
5. 更新 .gitignore 添加 *.pyc / __pycache__ / dist/ 等

Acceptance（贴执行结果给我）：
- [ ] git submodule status 显示 vendor/chatgpt2api 在某个 tag 上
- [ ] docker build -f Dockerfile.usage-service-plus -t cpa-manager-plus:dev . 成功
- [ ] docker run -p 18317:18317 cpa-manager-plus:dev 启动后：
      curl http://localhost:18317/health 返回 200
      docker exec <container> curl http://127.0.0.1:8000/docs 返回 200
- [ ] docker exec <container> ps -ef 能看到 cpa-manager 和 python 两个进程都在
- [ ] docker logs <container> 里 s6-overlay 没有 fatal、两个 service 都是 up 状态

Ask before doing（先问我，不要自己决定）：
1. chatgpt2api 锁定哪个 tag？（如果它没打 tag，问我用哪个 commit hash）
2. Python 进程是否要做 healthcheck？怎么判断它"准备好"了？
3. s6 服务的 logging 走 s6 自己的 logger 还是直接 stdout？
4. /data 目录所有权：chatgpt2api 的数据要不要也写进 /data 子目录？
5. 镜像是否要支持 linux/arm64？（上游 seakee/cpa-manager 支持）

不要做（超出范围）：
- 不要改 usage-service 的代码
- 不要改 chatgpt2api 的代码（用 Dockerfile COPY 覆盖在 Phase 2 做）
- 不要碰 React 前端代码
- 不要写 README / 文档
```

---

## Phase 2：usage-service 反代 + 鉴权桥接（Day 3）

```
【Phase 2 / 5】Go 反代层与统一鉴权

任务范围（只做这些）：
1. 在 usage-service/internal/httpapi/server.go 的 Handler() 里新增路由：
   a. /v1/images/generations、/v1/images/edits、/v1/chat/completions、
      /v1/responses、/v1/models  → 反代到 http://127.0.0.1:8000
   b. /v0/image/*  → 反代到 http://127.0.0.1:8000（去掉 /v0/image 前缀）
   c. 路由处理函数放新文件 usage-service/internal/httpapi/image_proxy.go
2. 鉴权桥接：
   a. 容器启动时由 s6 init 脚本生成 64 字节随机字符串写入 /run/chatgpt2api_internal_key
   b. chatgpt2api Python 进程通过环境变量 CHATGPT2API_AUTH_KEY 读这个值
   c. usage-service 启动时也读这个文件，在反代时强制覆盖 Authorization header 为
      "Bearer <internal_key>"
   d. usage-service 对外的鉴权：复用现有的 CPA Management Key 验证逻辑
      （参考 server.go 里现有 /v0/management/* 的 token 提取/校验代码）
3. 写 patch 文件 docker/patches/cpa_service.py.patch：
   a. 让 chatgpt2api 的 services/cpa_service.py 在启动时优先从环境变量
      CPA_BASE_URL + CPA_MANAGEMENT_KEY 读取 CPA 配置，写入 CPA_CONFIG_FILE
      （如果环境变量为空才回退到现有行为）
4. Dockerfile 里 COPY 这个 patch，在 py-builder 阶段 apply
5. s6 的 cpa-manager 服务启动前要等 chatgpt2api 的 /docs 返回 200（dependency）

Acceptance（贴执行结果给我）：
- [ ] go build ./usage-service/... 通过
- [ ] go test ./usage-service/... 通过（已有测试不能因为你的改动挂掉）
- [ ] 重新 build 镜像启动后：
      curl -H "Authorization: Bearer <CPA_MGMT_KEY>" http://localhost:18317/v1/models
      返回 chatgpt2api 的 model 列表（gpt-image-1/gpt-image-2）
- [ ] 不带 Authorization 时返回 401（来自 usage-service 而不是 chatgpt2api）
- [ ] 带错误的 Authorization 时返回 401
- [ ] docker exec <container> 看不到环境变量里的 CHATGPT2API_AUTH_KEY
      （它应该是文件传递，避免 docker inspect 泄露）
- [ ] 给 image_proxy.go 写至少 3 个单元测试（鉴权通过 / 鉴权失败 / 路径前缀剥离）

Ask before doing：
1. CPA Management Key 在 usage-service 里现在是怎么存的？（让我看一下相关代码片段确认）
2. 如果 chatgpt2api Python 进程没启动完成，反代请求应该返回 503 还是排队等？
3. /v0/image/* 这个前缀你觉得合理还是用别的（比如 /v0/management/image/*）？
4. patch 文件方式 vs 在 Dockerfile 里 sed 替换，哪种你更想要？

不要做：
- 不要重写 chatgpt2api Python 代码，只能 patch
- 不要碰 usage 相关的代码（collector、resp 等）
- 不要改任何 React 代码
- 不要新增任何对外端口（仍然只暴露 18317）
```

---

## Phase 3：React 图片号池页面（Day 4-5）

```
【Phase 3 / 5】前端 Image Pool 管理页

任务范围（只做这些）：
1. 在 src/pages/ 新建 ImagePool/ 目录，包含：
   a. ImagePoolPage.tsx：路由 /image-pool
   b. 子组件：AccountTable.tsx, ImportFromCPAButton.tsx, AccountStatusBadge.tsx
2. 在 src/services/api/ 新建 imagePool.ts，封装：
   - listAccounts() → GET /v0/image/accounts
   - syncFromCPA() → POST /v0/image/accounts/sync-from-cpa
   - refreshAccounts(tokens) → POST /v0/image/accounts/refresh
   - deleteAccounts(tokens) → DELETE /v0/image/accounts
   （所有调用复用现有 axios 实例和 baseURL 逻辑，鉴权 header 自动带 CPA Management Key）
3. 在 src/stores/ 新建 imagePoolStore.ts（Zustand），管理列表/loading/筛选状态
4. 在 src/components/layout/MainLayout.tsx 侧边栏加入口"Image Pool"
5. 在 src/router/ 加路由
6. 在 src/i18n/locales/ 加中英文 key（沿用现有 i18n 风格）
7. UI 风格严格遵循现有 CPA-Manager 设计 token：
   - 颜色用 src/styles/ 已有变量
   - 表格用 src/components/ui/ 已有组件
   - 不要引入新的 UI 库

Acceptance（贴执行结果给我）：
- [ ] npm run type-check 通过
- [ ] npm run lint 通过
- [ ] npm run build 通过（仍然是单文件 HTML）
- [ ] 在容器里跑起来访问 /image-pool，能看到表格（即使列表为空）
- [ ] 点 "Sync from CPA" 按钮后会调到 /v0/image/accounts/sync-from-cpa
      （后端这个 endpoint 可能还没实现，请求 404 OK，关键是前端调用对）
- [ ] 切换语言（中/英）页面文本都跟着切
- [ ] 移动端 viewport（375px）下表格不溢出

Ask before doing：
1. 给我看一下 src/components/ui/ 现有的 Table 组件签名，确认我用法对
2. 现有的 i18n key 命名规范是什么？snake_case 还是 camelCase？嵌套层级？
3. 侧边栏目前的菜单项数据结构（菜单项配置写在哪个文件）？
4. axios 实例的鉴权 interceptor 在哪？我要确认我不需要手动加 header
5. 移动端适配的断点用什么？（看 tailwind.config.js 还是 SCSS 变量）

不要做：
- 不要做"在线画图"页面（Phase 4 才做）
- 不要做"Image Manager / 图库"（暂时不需要）
- 不要改任何 Go 代码
- 不要改任何 Python 代码
- 后端 /v0/image/accounts/* 的具体实现不是这个 phase 的工作
  （你只需要确保前端调用 URL/方法/请求体格式跟我下面给的契约一致）

API 契约（前端按这个调用，后端 Phase 4/5 再实现）：
GET /v0/image/accounts
  → 200 { accounts: [{access_token, email, type, status, quota, last_refreshed_at}] }

POST /v0/image/accounts/sync-from-cpa
  body: { pool_id?: string }  // 不传则同步所有 CPA pool
  → 202 { job_id }

POST /v0/image/accounts/refresh
  body: { access_tokens: string[] }
  → 200 { refreshed: number }

DELETE /v0/image/accounts
  body: { access_tokens: string[] }
  → 200 { deleted: number }
```

---

## Phase 4：在线画图页 + 后端 sync 逻辑（Day 6）

```
【Phase 4 / 5】Online Draw 页 + Sync from CPA 后端实现

任务范围（只做这些）：

前端部分：
1. src/pages/ImagePool/OnlineDrawPage.tsx：路由 /image-pool/draw
2. 三个 tab：文生图 / 编辑图 / 多图组图
3. 调用 /v1/images/generations 和 /v1/images/edits
4. 结果本地保存到 IndexedDB（参考 chatgpt2api 自己 web/src/store/image-conversations.ts）
5. 侧边栏入口加二级菜单

后端部分（usage-service Go 侧）：
1. 实现 POST /v0/image/accounts/sync-from-cpa：
   a. 从 SQLite 读出当前已配置的 CPA URL + Management Key
   b. 调 CPA 的 /v0/management/auth-files 列出所有 auth 文件
   c. 对每个文件调 /v0/management/auth-files/download 拿 access_token
   d. POST 到 chatgpt2api 内部 /accounts/bulk-import（如果 chatgpt2api 没这个
      endpoint，问我是不是要 patch 一个出来）
   e. 异步执行，立即返回 job_id；提供 GET /v0/image/jobs/{id} 查询进度
2. 实现 GET /v0/image/accounts → 反代到 chatgpt2api 现有的账号列表 endpoint
3. 实现 POST /v0/image/accounts/refresh、DELETE /v0/image/accounts → 同上

Acceptance：
- [ ] 前端三个 tab 都能渲染，能上传图片预览
- [ ] 文生图按钮真的发请求到 /v1/images/generations，能拿到 b64_json
      （需要至少一个真实可用账号在号池里）
- [ ] Sync from CPA 跑完后，刷新 /image-pool 能看到 CPA 里的账号
- [ ] 重复点 Sync from CPA 不会重复导入相同 access_token
- [ ] 进度查询 endpoint 能正确返回 pending / running / completed
- [ ] 全部测试：go test + npm run test 都通过

Ask before doing：
1. 我之前在 chatgpt2api 代码里看到 services/cpa_service.py 已经有 cpa_import_service
   这个类，是不是直接复用？还是我们走自己的反代逻辑？
2. 异步 job 的状态存哪？SQLite 新表 vs 内存 map？崩溃恢复要不要支持？
3. IndexedDB 的库用什么？（idb? dexie? 还是原生）
4. 在线画图的图片要不要也走 usage-service 中转保存到 /data？还是只存浏览器本地？

不要做：
- 不要碰已有的 usage 统计逻辑
- 不要碰 React 已有的页面
- 不要做"图库管理"页面（用户要再说）
```

---

## Phase 5：文档 + Compose + 发布（Day 7）

```
【Phase 5 / 5】用户文档与发布物

任务范围（只做这些）：
1. 在仓库根目录写 README.md（中文为主，附英文段落）：
   - 一句话定位
   - 跟 upstream seakee/CPA-Manager 的区别（多了什么）
   - Quick Start（docker run 一条命令）
   - 配置说明：环境变量列表（包括新增的 IMAGE_* 相关项）
   - FAQ：跟原版 chatgpt2api 的关系、为什么内存多了 150MB、跟上游同步策略
2. 写 docker-compose.yml（生产用）和 docker-compose.dev.yml（开发用）
3. 写 .github/workflows/release.yml：
   - 复用上游 seakee 的 release 流程
   - 镜像 tag <你的 dockerhub>/cpa-manager-plus:vX.Y.Z 和 :latest
   - 支持 amd64 + arm64
4. 写 CHANGELOG.md 第一条 v0.1.0 列出所有 phase 做的事
5. 在 README 显眼位置放一张 architecture 图（mermaid 即可）

Acceptance：
- [ ] 在一台干净的服务器上，按 README Quick Start 一条命令跑通
- [ ] 镜像大小 < 600MB（不算就告警，让我决定要不要瘦身）
- [ ] release.yml 推 v0.1.0-rc1 tag 后能成功 push 到 dockerhub
- [ ] README 里所有 anchor 链接、图片链接都能打开

Ask before doing：
1. 你的 dockerhub 用户名是什么？要不要也推到 ghcr.io？
2. CHANGELOG 用 Keep a Changelog 风格还是 conventional commits 自动生成？
3. 架构图里要不要画出 chatgpt2api 这个内部进程？还是完全隐藏（对用户透明）？
4. README 要不要把"为什么不直接合并到上游 PR"这件事说清楚？

不要做：
- 不要写营销话术
- 不要列对比表"我们 vs xx"
- 不要加 emoji（除非用户特别要求）
- 不要承诺技术保证（如"100% 兼容 OpenAI API"）
```

---

## 全过程的"刹车"指令

如果某个 phase 跑到一半 Claude Code 开始：
- 跨 phase 改文件
- 自己加新依赖没问你
- 一次性 commit 几十个文件
- 改了你没让它改的目录

复制贴这段：

```
停。你超出了当前 phase 的范围。请：
1. git status 列出你改了哪些文件
2. 解释每个超范围改动的原因
3. 等我确认后再继续，或者 git restore 回滚
不要继续往前走。
```

---

## 给 Claude Code 的元 prompt（每次会话开头加一行）

```
请在开始任何工作前，先 git log --oneline -20 + git status，
告诉我当前仓库状态。如果有未提交改动，提示我并等待。
```

这一条能省下你 80% 的"哎我之前改的东西被冲掉了"的尴尬。
