# rc_jixiang — API 通知投递服务

## 一、问题理解

企业内部多个业务系统，在关键事件发生时（用户注册、订单支付、商品购买等），需要调用外部供应商的 HTTPS API 进行通知。核心挑战在于：

1. **解耦**：业务系统不应等待外部 API 的响应，也不应因外部 API 抖动而影响自身稳定性。
2. **可靠性**：通知请求必须稳定送达，不能因进程崩溃或外部 API 短暂不可用而丢失。
3. **异构适配**：不同供应商的 URL、Header、Body 格式各不相同，服务本身不应感知这些差异。

---

## 二、系统边界

### 本系统解决的问题

- 接收业务系统提交的通知任务，立即持久化，返回 202 Accepted（异步投递）
- 以**至少一次（at-least-once）**语义向外部 API 投递 HTTP 通知
- 投递失败后，按指数退避策略自动重试
- 超过最大重试次数的任务进入死信存储，供人工排查
- 进程崩溃重启后，自动恢复未完成的任务（不丢失）
- 提供任务状态查询接口，供运维排查

### 本系统**不**解决的问题（及原因）

| 不解决的问题 | 原因 |
|---|---|
| 入站请求认证/鉴权 | MVP 阶段假设内网信任；鉴权应由 API 网关或 mTLS 层处理，不在本服务职责范围内 |
| 供应商 API 的幂等性保证 | at-least-once 语义下可能出现重复投递；幂等性是供应商侧的责任，本服务不侵入 |
| 消息去重（idempotency key） | 业务系统可以在请求中带 `idempotency_key`，但 MVP 阶段不做强制唯一性校验 |
| 供应商响应内容解析 | 只关心 HTTP 状态码是否为 2xx；不解析响应 body，因为业务系统不需要关心返回值 |
| 分布式多实例协调 | SQLite 单写者模型只支持单节点；多实例场景需演进到 Postgres |
| 告警与监控 | 依赖结构化日志；告警应由外部监控系统（如 Prometheus/Grafana）消费日志实现 |

---

## 三、整体架构

```
业务系统
  │  POST /notifications
  ▼
[HTTP Server]  ──── 202 Accepted ──▶ 业务系统（立即返回）
  │  立即写入
  ▼
[SQLite 持久化队列]
  │  轮询 (2s)
  ▼
[Dispatcher 调度器]  →  [Worker Pool (N goroutines)]  →  [Deliverer]  →  外部供应商 HTTPS API
```

**关键流程：**
- 业务系统调用 `POST /notifications` 后，任务立即写入 SQLite 并返回 202，投递完全异步。
- Dispatcher 每隔 2 秒轮询数据库，将 `status=pending` 且 `next_retry_at <= now` 的任务原子性地更新为 `processing`，分发给 Worker。
- Worker 发起实际的 HTTPS 请求：成功则标记 `done`，失败则按退避策略设置 `next_retry_at` 重新入队。
- 达到最大重试次数的任务移入 `dead_letters` 表，标记 `failed`。

---

## 四、可靠性与失败处理

### 投递语义：至少一次（At-Least-Once）

选择 at-least-once 而非 exactly-once，原因如下：
- Exactly-once 需要与外部供应商建立两阶段确认协议，或依赖供应商提供幂等接口并在本系统存储已投递记录，复杂度显著增加。
- 绝大多数外部 API（广告系统、CRM、库存系统）的标准做法就是支持重复通知的幂等处理。
- 对于本场景，at-least-once 是工程上更合适的取舍。

### 崩溃恢复机制

任务状态流转：`pending` → `processing` → `done / failed`

**关键场景：** 进程在投递成功但写入 `done` 之前崩溃。

**处理方式：** 服务启动时执行：
```sql
UPDATE notifications SET status='pending' WHERE status='processing'
```
所有卡在 `processing` 的任务重置为 `pending`，会被重新投递。这是 at-least-once 语义的核心实现。

### 重试策略：指数退避

| 重试次数 | 等待时间 |
|---|---|
| 第 1 次失败后 | 2 秒 |
| 第 2 次失败后 | 4 秒 |
| 第 3 次失败后 | 8 秒 |
| 第 4 次失败后 | 16 秒 |
| 超出最大次数（默认 5）| 进入死信 |
| 上限 | 10 分钟 |

公式：`delay = min(2^attempt × 2s, 10min)`

### 外部系统长期不可用的处理

- 任务按退避策略持续重试，不会无限占用 Worker 资源（`next_retry_at` 推迟使任务暂时不被轮询）。
- 达到 `max_attempts` 后移入 `dead_letters`，主队列不再重试，避免无效资源消耗。
- 运维可通过 `GET /dead-letters` 查看失败任务，必要时手动重新投递。

---

## 五、关键设计决策与取舍

### 决策 1：调用方提供完整 URL/Headers/Body，而非服务维护供应商配置

**选择：** 调用方在请求中携带完整的 `url`、`headers`、`body`，本服务不感知供应商差异。

**好处：** 零配置管理，新增供应商无需改动本服务；服务职责单一，只做可靠投递。

**代价：** API 密钥等敏感信息会存入 SQLite。MVP 阶段可接受；生产环境应将供应商配置（含密钥）独立存储，本服务在投递时查询配置并组装请求，避免密钥落入任务队列。

### 决策 2：SQLite 作为持久化队列，而非外部消息队列

**选择：** SQLite（WAL 模式）

**理由：** 零外部依赖，单二进制部署，本地调试简单。WAL 模式下 SQLite 可轻松处理每秒数千任务，满足 MVP 规模。

**不采纳外部 MQ（Redis/RabbitMQ/Kafka）的原因：** 增加了运维复杂度和部署依赖，对 MVP 是过度设计。

**演进路径：** 需要多实例水平扩展时，切换到 PostgreSQL，使用 `SELECT FOR UPDATE SKIP LOCKED`，无需引入消息中间件即可支持多节点。

### 决策 3：轮询而非事件驱动

**选择：** 2 秒轮询间隔

**理由：** SQLite 不支持 LISTEN/NOTIFY。2 秒延迟对异步通知场景完全可接受。

**演进方向：** HTTP 接收任务后通过内部 channel 立即通知 Dispatcher，可将延迟降至毫秒级。

---

## 六、API 接口

### POST /notifications — 提交通知任务

**Request Body:**
```json
{
  "vendor_id": "crm_system",
  "url": "https://api.crm-vendor.com/contacts/update",
  "method": "POST",
  "headers": {
    "Authorization": "Bearer token123",
    "Content-Type": "application/json"
  },
  "body": "{\"contact_id\":\"abc\",\"status\":\"paid\"}",
  "max_attempts": 5
}
```

**Response 202 Accepted:**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "message": "notification accepted"
}
```

### GET /notifications/{id} — 查询任务状态

### GET /notifications?status=failed — 列出任务（支持状态过滤）

### GET /dead-letters — 查看永久失败的任务

### GET /health — 健康检查

---

## 七、环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `8080` | HTTP 监听端口 |
| `DB_PATH` | `rc_jixiang.db` | SQLite 数据库文件路径 |
| `WORKER_COUNT` | `5` | 并发投递 Worker 数量 |
| `POLL_INTERVAL_SECONDS` | `2` | Dispatcher 轮询间隔（秒） |
| `HTTP_TIMEOUT_SECONDS` | `30` | 每次投递请求的超时时间（秒） |

---

## 八、本地运行

```bash
# 安装依赖
go mod tidy

# 启动服务
go run main.go

# 提交一个通知任务
curl -X POST http://localhost:8080/notifications \
  -H 'Content-Type: application/json' \
  -d '{
    "vendor_id": "test",
    "url": "https://httpbin.org/post",
    "body": "{\"hello\":\"world\"}"
  }'

# 查询任务状态
curl http://localhost:8080/notifications/{id}

# 查看死信
curl http://localhost:8080/dead-letters
```

---

## 九、演进路径

| 阶段 | 变化 | 技术手段 |
|---|---|---|
| MVP（当前） | 单节点，SQLite | 本文档描述的架构 |
| 中等规模 | 多实例，共享存储 | 迁移至 PostgreSQL，`SELECT FOR UPDATE SKIP LOCKED` |
| 大规模 | 高吞吐，解耦存储 | 引入 Kafka/RabbitMQ，本服务演变为纯 Consumer |
| 安全加固 | 密钥管理 | 供应商配置独立存储，本服务不接触明文密钥 |

---

## 十、AI 使用说明

### AI 提供帮助的地方
- 生成 SQLite schema 初稿和 `ClaimPending` 的原子 UPDATE 写法
- 建议使用 `log/slog`（Go 1.21 stdlib）替代第三方日志库，减少依赖
- 梳理 at-least-once vs exactly-once 的工程权衡逻辑

### AI 给出但未采纳的建议
- **引入 Redis 作为任务队列**：AI 建议用 Redis List + BLPOP 实现队列，实时性更好。未采纳原因：增加外部依赖，对 MVP 是过度设计，SQLite 轮询完全够用。
- **Jitter 加入退避公式**：AI 建议在退避时间上加随机抖动防止惊群。未采纳原因：本服务是单实例单队列，不存在多个 Consumer 同时重试同一外部 API 的场景，jitter 意义不大。
- **供应商配置注册表**：AI 建议在服务内部维护供应商配置，调用方只传 `vendor_id`。未采纳原因：MVP 阶段增加了不必要的 CRUD 接口和配置管理负担。

### 关键决策由自己做出
- **系统边界的划定**：明确本服务不做鉴权、不保证幂等性、不解析供应商响应。
- **SQLite 而非 MQ**：基于"最小依赖原则"和 MVP 规模的工程判断。
- **at-least-once 而非 exactly-once**：基于外部 API 标准实践和工程复杂度的取舍。
