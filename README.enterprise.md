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
* **大表按月声明式分区与冷归档**：支持对 `activity`、`ent_audit`、`llm_records` 自动预建按月分区，并将超期历史分区流式导出为 gzipped JSONL 存入 `BlobStore` 后安全清理。
* **分布式流量索引**：`traffic/` 索引存储接口化，单机默认 SQLite，分布式模式自动切换为 PostgreSQL 元数据索引 + `BlobStore` 按 SHA-256 去重正文。
* **轻量级 Fan-out / Fan-in 批处理**：基于 `batch_jobs` / `batch_shards` 将大批量目标（如资产测绘 `asset_recon`、批量复测 `finding_retest`）去重切片派发给多个 Worker 并行执行，收尾自动归并去重结果。
* **项目级多租户配额**：支持为每个项目配置最大并发任务数（`max_concurrent_tasks`）及日/月 LLM Token 预算（`daily_token_limit` / `monthly_token_limit`），超限时拦截返回 `429` 并记录审计。

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
