# ARTEX 企业版（Enterprise Edition）

> 面向企业安全团队的多用户协作、权限隔离与分布式执行架构方案，基于单一 Go 代码库 + PostgreSQL 16 + 对象存储（S3/MinIO），同时兼容**单机一键私有化部署**与**多副本控制面 + 跨网段分布式 Worker 集群**。

---

## 一、企业版整体架构

```mermaid
flowchart LR
    U["企业员工 / 管理员 / 审计员"] -->|"HTTPS + OIDC SSO"| LB["反向代理 / 负载均衡"]
    LB --> API1["artex-api 节点 1\n(--mode=api)"]
    LB --> API2["artex-api 节点 2\n(--mode=api)"]

    API1 <-->|"LISTEN / NOTIFY\nEventBus & ControlEvent"| PG[("PostgreSQL 16\n业务库 · 队列 · 选主 · 按月分区")]
    API2 <-->|"LISTEN / NOTIFY\nEventBus & ControlEvent"| PG

    API1 <-->|"BlobStore"| S3[("S3 / MinIO\n原件 · 签发报告 · 流量正文 · 冷归档")]
    API2 <-->|"BlobStore"| S3

    W1["artex-worker\n(--worker-zone=default)"] <-->|"SKIP LOCKED 认领 & 心跳租约"| PG
    W2["artex-worker\n(--worker-zone=intranet)"] <-->|"SKIP LOCKED 认领 & 心跳租约"| PG
    W1 <-->|"直传产物与流量正文"| S3
    W2 <-->|"直传产物与流量正文"| S3
```

---

## 二、核心能力一览

### 1. 多用户身份、权限与合规闭环
* **标准企业 SSO（OIDC）**：支持 OIDC Discovery、JWKS 公钥缓存与自动轮换、PKCE S256 与标准声明强校验；登录事务持久化在 `ent_login_states`，支持多副本 API 回调。保留 `ARTEX_DEMO_IDP=1` 仅用于本地演示。
* **会话与传输加固**：`HttpOnly` + `SameSite=Lax` + HTTPS `Secure` Cookie；企业模式下禁用 `?token=` URL 传参；状态变更接口校验同源 `Origin/Referer`；`TouchSession` 采用 2 分钟写入节流，降低高频并发下的数据库写压力。
* **项目级隔离与职责分离**：按项目维护基础角色（负责人 / 成员 / 只读）与独立职责（审批 / 复核签发 / 审计）；报告版本签发强制回避原作者与来源贡献人。
* **脱敏与原件限时审批**：内置凭据脱敏规则（口令键值、PEM 私钥、JWT、云 AK/SK、Cookie/Authorization 头）；原始截图/流量默认受限，须经独立审批签发限时 `ent_original_grants`（含凭据原件须双授权），撤权、停用或归档时即时失效。
* **脱敏出站通知**：企业审批、加入请求、报告送审/退回/签发等事件统一经 `notify/` 出站投递，正文严格脱敏，不泄露自由文本理由或凭据。

### 2. 控制面无状态化与分布式执行池
* **跨节点事件总线（`EventBus`）**：抽象 `MemoryBus`（单机默认）与 `PGNotifyBus`（基于 PostgreSQL `LISTEN/NOTIFY`，支持多副本 API 跨节点 SSE 实时推送与 `ControlEvent` 下发）。
* **调度器分布式选主**：`Scheduler` 通过独立连接持有 PostgreSQL session 级 Advisory Lock，多副本部署时保证同一时刻仅 Leader 触发定时任务、租约回收与分区维护，故障自动接管。
* **分布式 `artex-worker` 节点池**：
  * 单一二进制通过 `--mode=all|api|worker` 切换角色。
  * 基于 `worker_nodes` 与 `execution_jobs`（`SELECT ... FOR UPDATE SKIP LOCKED`）实现无重复抢占、心跳续约（Lease）与超时自动重调度。
  * 支持 `--worker-zone` 网段标签路由（如 `default` / `dmz` / `intranet`），将不同网段的扫描任务精准调度至对应探针节点。

### 3. 对象存储、数据治理与并行编排
* **统一对象存储抽象（`storage.BlobStore` / `StreamBlobStore`）**：支持 `LocalFSStore`（单机默认，防路径穿越与符号链接逃逸）与 `S3Store`（兼容 MinIO / AWS S3 / OSS），解耦受限原件、签发报告、任务冷归档与流量正文。
* **大表按月声明式分区与冷归档**：支持对 `activity`、`ent_audit`、`llm_records` 自动预建按月分区，并将超期历史分区流式导出为 gzipped JSONL 存入 `BlobStore` 后安全清理（同步维护仪表盘日用量索引 `idx_act_result_created`）。
* **分布式流量索引**：`traffic/` 索引存储接口化，单机默认 SQLite（含后台异步 FTS 队列与 `(host, ts)` 复合索引），分布式模式自动切换为 PostgreSQL 元数据索引 + `BlobStore` 按 SHA-256 去重正文。
* **轻量级 Fan-out / Fan-in 批处理**：基于 `batch_jobs` / `batch_shards` 将大批量目标（如资产测绘 `asset_recon`、批量复测 `finding_retest`）去重切片派发给多个 Worker 并行执行，收尾自动归并去重结果。
* **项目级多租户配额**：支持为每个项目配置最大并发任务数（`max_concurrent_tasks`）及日/月 LLM Token 预算（`daily_token_limit` / `monthly_token_limit`），超限时拦截返回 `429` 并记录审计。

### 4. 安全加固、Agent 纵深防御与高级流量能力（BaaRai 融合特性）
* **纵深安全基线与子进程权限隔离**：
  * 启动后自动擦除环境变量中的数据库/OIDC/S3/LLM 凭据（Linux 支持 `SealProcessEnv` 清除内核环境块，同时保留 `--mode=worker` 节点配置）。
  * **Linux 执行器降权与隔离自检（`execpriv` + `isolation`）**：服务端以 root 运行时（如容器环境），Agent 启动的全部子进程（前后台 `Bash`、自定义命令/Python 工具、stdio MCP）统一降权至非特权 UID/GID（默认 `10001:10001`，设 `PR_SET_NO_NEW_PRIVS`）并重建最小化环境变量白名单，同时完整继承 `toolguard` 的 cgroup 内存/PIDs 与超时上限；内置文件工具基于 `os.Root` + 执行器身份严格限制在运行目录叶节点（`tasks/<id>/main`、`tasks/<id>/i<N>`、`sessions/<id>`）内，超大输出独立落盘至只读共享 `spill/`；启动前执行隔离自检探针，状态通过 `GET /api/version` 暴露。
  * 非企业单机模式首装校验一次性 `setup_token`；登录/改密按 IP 限流；全站两层安全响应头（不阻断 OIDC 跳转、SSE 与 `/mcp`）；归档自动跳过特殊设备文件并随包备份/恢复 `spill`；静态加密密钥 `secret.key` 独立存放于 `ARTEX_JWT_KEY_DIR`（与 `jwt.key` 同目录，远离可浏览数据目录）。
* **Agent 防注入、契约自进化（RSI）与证据可验证性**：
  * 非受信数据随机 Nonce 信封隔离；事实溯源标记（`source` / `origin`）；闭卷式审批裁判（与既有 `freeform` 及 Jev System One 共存）；`argv` 级精确命令拦截与危险前缀黑名单；任务专属测试凭据管理（`task_credentials`，口令经 `secret.key` 静态加密，前端详情页独立维护并通过工具按需读取）；`ask_operator` 人工协同提问与非目标来源域名告警标记。
  * 结构化 `set_goals` 目标提取契约（校验重试，仅在未配置 LLM 时回退原始描述）、提供商安全拒答识别（`providerstop`）与 `noa` 上下文预算前置拦截（`ErrNoaBudget` 免无效重试）；System One 辅助漏洞去重研判（`triage_profile_id` / `triage_min_confidence`，内网 SSRF 防护，支持 `force_register` 显式放行）；POE 证据原文逐字核验、报告证据强度质量建模（`report/quality` 过期提示、反证与置信依据、CSV 公式注入防护，并与人工 `remark` 和归并案件共存）；RSI 任务复盘经验注入（`lessons`）、能力缺口登记（`skill_gaps`）与任务完成结果量化（`task_outcome`）。
* **高级流量引擎与前端工作台**：支持 HAR 1.2 导入/导出（含截断/丢包统计提示）、WebSocket/SSE 流消息分页检视与二进制/Hex 载荷拉取（`StreamMessagesPanel`）、内联单包改包重放面板（`ReplayPanel`，支持一键叠加 `apply_rules` 在线验证改写规则与篡改脚本，与独立 Repeater 并存）、声明式流量改写规则（Rewrite Rules，敏感值静态加密）及受限 Go 沙箱篡改 Hook 全生命周期管理面板（`TamperHooksPanel`：匹配器构建、代码编辑、版本回滚、带 HMAC 脱敏轨迹的 Dry-run 试跑）；支持 Opt-in SOCKS5（TCP/UDP）与 Linux 透明 TCP 代理；附带 Chrome 抓包扩展（`extension/`）与可选容器级 DNS 出网白名单（`egress-guard/` + `docker-compose.egress.yml`）。
* **模型、可观测与运维扩展**：支持 7 种 LLM 协议共存（`openai`、`anthropic`、`openai-responses`、`gemini`、`jev`、`ollama`、`bedrock-gateway`）、任务级多 LLM 并发轮转（与运行中 Worker 并发热更新及 Worker 池并存）、`GET /api/ready` 就绪探针、全局公司视图切换、批量漏洞状态处置、资产范围通配符域名（`*.example.com`），以及可选的 Telegram 双向 Chat Bridge（企业模式下仅平台管理员可配置，与 `notify/enterprise.go` 脱敏通知彻底隔离）。

---

## 三、快速启动与部署方式

### 模式 1：单机本地演示（含演示身份源）
适合快速体验企业版完整权限与审批流程（沿用默认 `--mode=all`）：

```bash
export ARTEX_PG_DSN="postgres://artex:password@127.0.0.1:5432/artex?sslmode=disable"
export ARTEX_ENTERPRISE=1
export ARTEX_DEMO_IDP=1
export ARTEX_PUBLIC_ORIGIN="http://127.0.0.1:5173"

go run ./cmd/artex --mode=all --addr=:8787
```

### 模式 2：单机生产模式（接入真实 OIDC + S3/MinIO）
```bash
export ARTEX_PG_DSN="postgres://artex:password@pg.internal:5432/artex?sslmode=require"
export ARTEX_ENTERPRISE=1
export ARTEX_PUBLIC_ORIGIN="https://artex.corp.example"
export ARTEX_OIDC_ISSUER="https://idp.corp.example/realms/security"
export ARTEX_OIDC_CLIENT_ID="artex-enterprise"
export ARTEX_OIDC_CLIENT_SECRET="your-client-secret"

# 可选：接入 S3 / MinIO 对象存储（不配置则默认使用本地 restricted 目录）
export ARTEX_S3_ENDPOINT="https://minio.corp.example"
export ARTEX_S3_BUCKET="artex-prod"
export ARTEX_S3_REGION="us-east-1"
export ARTEX_S3_ACCESS_KEY="your-access-key"
export ARTEX_S3_SECRET_KEY="your-secret-key"

./artex --mode=all --addr=:8787
```

### 模式 3：分布式多节点部署（多副本 API + 多网段 Worker）
所有节点共享同一套 PostgreSQL 16 与 S3/MinIO：

**1. 启动控制面节点（可部署 2+ 个副本挂在负载均衡后）**
```bash
export ARTEX_PG_DSN="postgres://artex:password@pg.internal:5432/artex?sslmode=require"
export ARTEX_ENTERPRISE=1
export ARTEX_EVENT_BUS=pg
export ARTEX_TRAFFIC_STORE=pg
export ARTEX_PUBLIC_ORIGIN="https://artex.corp.example"
export ARTEX_OIDC_ISSUER="https://idp.corp.example/realms/security"
export ARTEX_OIDC_CLIENT_ID="artex-enterprise"
export ARTEX_OIDC_CLIENT_SECRET="your-client-secret"
# 加上 ARTEX_S3_* 对象存储配置

./artex --mode=api --addr=:8787
```

**2. 启动默认网段执行节点（Worker）**
```bash
export ARTEX_PG_DSN="postgres://artex:password@pg.internal:5432/artex?sslmode=require"
export ARTEX_EVENT_BUS=pg
export ARTEX_TRAFFIC_STORE=pg
# 加上 ARTEX_S3_* 对象存储配置

./artex --mode=worker --worker-zone=default --worker-concurrency=4
```

**3. 启动内网隔离网段执行节点（Worker）**
```bash
./artex --mode=worker --worker-id=intranet-node-01 --worker-zone=intranet --worker-concurrency=4
```

---

## 四、CLI 参数与环境变量速查

### 命令行参数（`cmd/artex`）

| 参数 | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `--mode` | `ARTEX_MODE` | `all` | 进程角色：`all`（单机一体）、`api`（控制面）、`worker`（执行节点） |
| `--addr` | — | `:8787` | HTTP API 监听地址（`worker` 模式下不监听） |
| `--data` | — | `./data` | 本地数据目录 |
| `--proxy` | — | `127.0.0.1:8788` | 流量录制代理监听地址（空字符串禁用） |
| `--worker-id` | `ARTEX_WORKER_ID` | `<hostname>-<rand>` | Worker 节点唯一标识 |
| `--worker-zone` | `ARTEX_WORKER_ZONE` | `default` | Worker 认领任务的网段标签 |
| `--worker-concurrency` | `ARTEX_WORKER_CONCURRENCY` | `4` | 单 Worker 最大并发作业数 |
| `--exec-uid` / `--exec-gid` | — | `10001` / `10001` | Linux root 启动时 Agent 子进程降权目标 UID/GID（`--exec-uid=-1` 显式关闭降权） |
| `--dev-insecure-no-isolation` | — | `false` | 仅限开发排障：允许 Linux root 在隔离自检失败时降级启动 |

### 核心环境变量

| 环境变量 | 说明 |
| :--- | :--- |
| `ARTEX_PG_DSN` | PostgreSQL 16 连接串（必填） |
| `ARTEX_ENTERPRISE` | 设为 `1` 开启企业模式（关闭共享密码登录，启用项目级权限与 SSO） |
| `ARTEX_PUBLIC_ORIGIN` | 对外访问根地址（如 `https://artex.corp.example`），用于 OIDC 回调、Cookie `Secure` 与同源校验 |
| `ARTEX_OIDC_ISSUER` / `ARTEX_OIDC_CLIENT_ID` / `ARTEX_OIDC_CLIENT_SECRET` | 标准 OIDC 身份源配置 |
| `ARTEX_DEMO_IDP` | 设为 `1` 启用本机演示身份源（仅限本地测试） |
| `ARTEX_EVENT_BUS` | 事件总线后端：`memory`（单机默认）或 `pg`（`api`/`worker` 模式默认启用 PG `LISTEN/NOTIFY`） |
| `ARTEX_TRAFFIC_STORE` | 流量索引后端：`sqlite`（单机默认）或 `pg`（分布式模式默认启用 PG + `BlobStore`） |
| `ARTEX_S3_ENDPOINT` / `ARTEX_S3_BUCKET` / `ARTEX_S3_REGION` / `ARTEX_S3_ACCESS_KEY` / `ARTEX_S3_SECRET_KEY` | S3/MinIO 对象存储配置（全部配置时自动切换为 `S3Store`） |
| `ARTEX_PARTITION_RETENTION_DAYS` | 分区保留天数（默认 `0` 不自动清理；设置正整数后 Leader 定期将过期分区归档至 `BlobStore` 并删除） |

### 安全加固与 Opt-in 扩展环境变量（以下高影响开关均默认关闭）

| 环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `ARTEX_BIND_ADDR` | `127.0.0.1` | Docker Compose 宿主机监听地址（远程访问需显式设为 `0.0.0.0`） |
| `ARTEX_JWT_KEY_DIR` | 进程基目录 | `jwt.key` 与静态加密密钥 `secret.key` 存放目录（与工作区 `data/` 隔离） |
| `ARTEX_SECRET_KEY` | 空 | 可选外部静态加密主密钥；为空时自动生成于 `ARTEX_JWT_KEY_DIR/secret.key` |
| `ARTEX_TRUSTED_PROXIES` | 空 | 逗号分隔可信反代 CIDR/IP，仅来自可信代理的 `X-Forwarded-Proto: https` 会下发 HSTS |
| `ARTEX_NOISE_PROFILE` | `off` | 目标噪音限速档位：`off` \| `quiet` \| `balanced` \| `aggressive`（可在设置页热切换） |
| `ARTEX_TARGET_USER_AGENT` | 空 | 在启用噪音档位时覆盖缺省目标请求 `User-Agent` |
| `ARTEX_TOOL_RAG` | 关闭 | 设为 `1` 启用按意图动态裁剪工具列表的 Tool RAG |
| `ARTEX_TASK_BUDGET_TOKENS` / `ARTEX_TASK_BUDGET_TURNS` | `0`（不限） | 任务级层级 Token / 轮次预算上限（与企业项目配额独立互补） |
| `ARTEX_INTENT_BUDGET_TOKENS` / `ARTEX_INTENT_BUDGET_TURNS` | `0`（不限） | 单意图层级 Token / 轮次预算上限 |
| `ARTEX_TAMPER_HOOKS_ENABLED` | 关闭 | 设为 `1` 启用受限 Go 沙箱流量篡改 Hook（`traffic/hookvm`） |
| `ARTEX_SOCKS5_ENABLED` / `ARTEX_SOCKS5_ADDR` | 关闭 / `127.0.0.1:1080` | 设为 `1` 启用回环 SOCKS5 入站代理与原始 TCP 流录制 |
| `ARTEX_SOCKS5_UDP_ENABLED` | 关闭 | 设为 `1` 启用 SOCKS5 `UDP ASSOCIATE` 与原始 UDP 录制 |
| `ARTEX_TRANSPARENT_TCP_ENABLED` / `ARTEX_TRANSPARENT_TCP_ADDR` | 关闭 / `127.0.0.1:1081` | Linux 专用，设为 `1` 启用透明 TCP 入站录制（需宿主机重定向规则） |
| `ARTEX_CONTEXT_TELEMETRY` | 关闭 | 设为 `1` 记录脱敏后的每轮 LLM 上下文大小遥测至 `llm_context` 表 |
| `ARTEX_CACHE_PREFIX` | 关闭 | 设为 `1` 启用 Worker System Prompt 静态/动态缓存前缀拆分 |

### 升级与兼容注意事项
* **单机首装 vs 企业 SSO**：`ARTEX_ENTERPRISE=1` 时跳过一次性 `setup_token`；单机全新部署初始化 `/setup` 需从服务端标准错误输出读取 `Setup token`。
* **密钥迁移**：首次升级启动时若发现旧路径 `data/secret.key`，会自动迁移至 `ARTEX_JWT_KEY_DIR/secret.key` 并清理旧文件。
* **数据库幂等升级**：所有新增表、列与索引均通过 `db/schema.sql` 幂等追加；`llm_profiles_format_check` 同时保留全部 7 种模型格式，既有按月分区与企业表不受影响。

---

## 五、新增企业版与分布式核心 API

* **身份与企业工作台**：`/api/enterprise/login`、`/api/enterprise/callback`、`/api/enterprise/me`、`/api/enterprise/workbench`、`/api/enterprise/todos`
* **项目成员、加入与配额**：
  * `POST /api/enterprise/join`、`POST /api/enterprise/join/{id}/approve|reject`
  * `GET /api/enterprise/projects/{id}/members`、`POST /api/enterprise/projects/{id}/members`
  * `GET /api/enterprise/projects/{id}/quota`、`PUT /api/enterprise/projects/{id}/quota`（平台管理员）
* **报告签发与原件限时审批**：
  * `POST /api/enterprise/reports/{id}/versions/{vid}/submit|return|withdraw|issue`
  * `POST /api/enterprise/reports/{id}/original-requests`
  * `POST /api/enterprise/original-requests/{id}/approve|reject`
  * `GET /api/enterprise/originals/{id}`
* **分布式集群与批处理编排**：
  * `GET /api/workers`（平台管理员查看在线 Worker 节点及各 zone 队列状态）
  * `POST /api/batches`、`GET /api/batches/{id}`、`POST /api/batches/{id}/cancel`

---

## 六、测试验证

```bash
# 全量编译检查
go build ./...

# 无数据库依赖的核心单元测试
go test ./cmd/artex/... ./storage/... ./notify/... ./traffic/...

# 全量集成测试（需提供本地 PostgreSQL 16 测试实例）
ARTEX_PG_DSN="postgres://artex:password@127.0.0.1:5432/artex_test?sslmode=disable" go test ./...
```
