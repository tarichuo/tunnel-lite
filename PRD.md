# Tunnel Lite 产品需求文档

**版本：** V1.0  
**产品定位：** 轻量级 TCP 内网穿透及端口映射工具  
**部署环境：** 2GB 内存、低存储空间的 Linux 公网服务器  
**技术方向：** Go + SQLite + HTML/JS

## 1. 产品介绍

Tunnel Lite 是一个轻量级内网穿透工具。

用户在公网服务器部署服务端，在本地机器部署 Agent，即可将本地机器的 TCP 服务映射到公网服务器的指定端口。

系统提供：

1. 本地 Agent：连接公网服务器，申请和维护端口映射。
2. 公网 Server：提供 API、端口监听和 TCP 数据转发。
3. Web 管理页面：查看当前映射服务，以及创建和下线映射。

不依赖 MySQL、Redis、消息队列等外部服务。

## 2. 整体架构

```text
             外部用户
                 |
                 |
           公网 IP:18080
                 |
       +--------------------+
       |    公网服务器       |
       |                    |
       |    Tunnel Server   |
       |                    |
       |    REST API        |
       |    TCP 转发         |
       |    Web 管理页面     |
       |                    |
       |    SQLite          |
       +--------------------+
                 |
                 |
           TCP 长连接
                 |
       +--------------------+
       |     本地机器        |
       |                    |
       |    Tunnel Agent    |
       |                    |
       |    127.0.0.1:3000  |
       +--------------------+
```

所有服务端功能集成在一个 Go 程序中。

SQLite 负责持久化映射配置和 Agent 信息，不需要单独部署数据库服务。

## 3. 核心功能

### 3.1 本地 Agent

Agent 是运行在本地机器上的轻量级程序。

主要功能：

- 连接公网服务器。
- 使用 Token 进行身份认证。
- 保持 TCP 长连接。
- 定时发送心跳。
- 申请公网端口。
- 接收服务端转发请求。
- 连接本地指定 IP 和端口。
- 双向转发 TCP 数据。
- 网络断开后自动重连。
- 重连后恢复有效映射。

一个 Agent 可以同时创建多个映射。

Agent 配置示例：

```yaml
server: "1.2.3.4:7001"
token: "your-token"

name: "my-server"

```

Agent 启动：

```bash
./tunnel-agent -c config.yaml
```

### 3.2 公网 Server

公网 Server 是系统的核心。

主要功能：

- 提供 HTTP REST API。
- 提供 Web 管理页面。
- 管理 Agent 连接。
- 管理公网端口分配。
- 监听公网 TCP 端口。
- 将公网 TCP 请求转发给对应 Agent。
- 维护映射状态。
- 检测 Agent 在线状态。
- 下线映射并释放端口。
- 使用 SQLite 保存数据。

服务端配置：

```yaml
server:
  api_port: 7000
  control_port: 7001

port_pool:
  start: 10000
  end: 20000

heartbeat:
  interval: 15
  timeout: 45

database:
  path: "./data/tunnel.db"
```

公网服务器需要开放：

- 7000：API 和 Web 管理页面。
- 7001：Agent 控制连接。
- 10000-20000：公网 TCP 转发端口。

### 3.3 端口映射管理

每个映射包含以下信息：

| 字段 | 说明 |
|---|---|
| ID | 映射唯一 ID |
| 名称 | 用户自定义名称 |
| Agent | 所属本地机器 |
| 协议 | V1.0 固定 TCP |
| 本地 IP | 例如 127.0.0.1 |
| 本地端口 | 例如 3000 |
| 公网 IP | 公网服务器 IP |
| 公网端口 | 自动分配或指定 |
| 状态 | 运行中、异常、已下线 |
| 创建时间 | 创建时间 |

支持两种端口分配方式：

- 自动分配：服务端从端口池中分配可用端口。
- 指定端口：用户指定公网端口，如果被占用则返回错误。

### 3.4 映射生命周期

映射状态只保留必要的几种：

| 状态 | 说明 |
|---|---|
| STARTING | 正在创建 |
| ACTIVE | 正常运行 |
| OFFLINE | Agent 断开 |
| STOPPED | 已主动下线 |
| ERROR | 创建或运行失败 |

状态变化：

```text
STARTING -> ACTIVE
              |
              +----> OFFLINE
              |         |
              |         +----> ACTIVE
              |
              +----> STOPPED

STARTING -> ERROR
```

Agent 临时断开时不释放公网端口，重新连接后恢复映射。

用户主动下线时释放端口。

## 4. API 设计

API 统一使用 JSON。

基础地址：

```text
http://公网IP:7000/api/v1
```

所有管理 API 使用 Token 认证。

### 4.1 创建映射

```http
POST /api/v1/tunnels
```

请求：

```json
{
  "agent_id": "agent_001",
  "name": "my-web",
  "local_address": "127.0.0.1",
  "local_port": 3000,
  "remote_port": 0
}
```

`remote_port` 为 0 时自动分配。

响应：

```json
{
  "id": "tunnel_001",
  "public_address": "1.2.3.4",
  "public_port": 18080,
  "status": "ACTIVE"
}
```

### 4.2 获取映射列表

```http
GET /api/v1/tunnels
```

返回当前所有映射。

### 4.3 获取映射详情

```http
GET /api/v1/tunnels/{id}
```

### 4.4 下线映射

```http
DELETE /api/v1/tunnels/{id}
```

下线后关闭公网端口监听，并释放端口。

### 4.5 获取 Agent 列表

```http
GET /api/v1/agents
```

### 4.6 获取 Agent 详情

```http
GET /api/v1/agents/{id}
```

Agent 的注册和心跳通过控制连接完成，不需要单独提供复杂的管理接口。

## 5. Web 管理页面

Web 页面采用简单的单页管理界面。

不需要复杂的前端框架也可以，优先考虑轻量和易维护。

### 5.1 首页

展示：

- 在线 Agent 数量。
- 当前映射数量。
- 正常运行的映射数量。
- 异常映射数量。

### 5.2 映射列表

页面核心表格：

| 名称 | Agent | 本地地址 | 公网地址 | 状态 | 操作 |
|---|---|---|---|---|---|
| web | server-01 | 127.0.0.1:3000 | 1.2.3.4:18080 | ACTIVE | 下线 |
| api | server-01 | 127.0.0.1:8080 | 1.2.3.4:18081 | ACTIVE | 下线 |

支持：

- 查看映射列表。
- 创建映射。
- 复制公网地址。
- 查看映射详情。
- 下线映射。

### 5.3 Agent 列表

展示：

- Agent 名称。
- Agent ID。
- 在线状态。
- 最后心跳时间。
- 当前映射数量。

### 5.4 创建映射

创建表单：

- 选择 Agent。
- 输入映射名称。
- 输入本地 IP。
- 输入本地端口。
- 输入公网端口（可选）。

点击创建后调用 REST API。

### 5.5 状态刷新

前端每 5 秒请求一次 API，刷新映射和 Agent 状态。

V1.0 不需要 WebSocket。

## 6. 网络通信设计

采用简单的反向 TCP 隧道。

### 控制连接

Agent 主动连接公网服务器，保持一个 TCP 长连接。

用于：

- 身份认证。
- 心跳。
- 创建和下线映射。
- 通知 Agent 有新的公网连接。
- 同步映射配置。

### 数据连接

每个外部 TCP 请求建立独立的数据通道。

流程：

1. 外部用户访问公网 IP 和端口。
2. Server 接受 TCP 连接。
3. Server 找到对应映射。
4. Server 通过控制连接通知 Agent。
5. Agent 连接本地目标 IP 和端口。
6. Agent 建立独立数据连接。
7. Server 将外部连接和数据连接关联。
8. 双向转发数据。
9. 连接结束后释放资源。

控制连接与数据连接分离，避免一个数据连接异常影响整个 Agent。

所有连接使用 Connection ID 关联。

V1.0 使用简单的长度前缀二进制消息协议即可，不需要引入复杂的 RPC 框架。

## 7. 数据存储

使用 SQLite。

不需要 MySQL、Redis 等外部依赖。

只需要三张核心表：

**agents**

保存 Agent 信息、Token 哈希、在线状态和最后心跳时间。

**tunnels**

保存映射配置、公网端口、本地地址、映射状态和创建时间。

**operation_logs**

保存创建、下线等关键操作记录。

数据库文件：

```text
./data/tunnel.db
```

服务端启动时自动创建数据库和初始化表结构。

不需要额外部署数据库服务。

## 8. 安全要求

V1.0 只实现必要的安全措施：

- Agent 使用独立 Token 认证。
- 管理 API 使用独立管理 Token。
- Token 不以明文形式存入数据库。
- 控制连接使用 TLS。
- 数据连接需要验证对应的 Agent、Tunnel 和 Connection ID。
- 管理页面必须经过身份认证。
- 限制公网端口范围。
- 限制每个 Agent 的最大映射数量。
- 限制单个映射的并发连接数量。
- 限制本地目标地址，避免未授权访问其他内网资源。

**证书方面：** 不要求购买商业证书。可以使用自建 CA 生成服务端证书，并将 CA 证书配置到 Agent。后续也可以支持通过 ACME 自动获取免费证书。

不建议在生产环境中使用明文控制连接。

## 9. 技术选型

| 模块 | 技术 |
|---|---|
| 服务端 | Go |
| Agent | Go |
| HTTP API | Go 标准库 net/http |
| TCP 转发 | Go net |
| 数据库 | SQLite |
| 前端 | 原生 HTML + CSS + JavaScript |
| API 格式 | JSON |
| 配置文件 | YAML |
| 部署 | Linux 二进制程序 |

不引入：

- MySQL
- Redis
- RabbitMQ
- Kafka
- Kubernetes
- 微服务架构
- 独立前端服务器

## 10. 部署方式

公网服务器只需要一个程序：

```text
tunnel-server
```

启动：

```bash
./tunnel-server -c server.yaml
```

本地机器只需要一个程序：

```text
tunnel-agent
```

启动：

```bash
./tunnel-agent -c config.yaml
```

公网服务器目录：

```text
tunnel/
├── tunnel-server
├── server.yaml
├── data/
│   └── tunnel.db
└── logs/
```

Web 管理页面直接由 tunnel-server 提供静态文件和 API。

Agent 可以编译成 Linux、macOS、Windows 对应的独立二进制文件。

## 11. 资源要求

目标是能够稳定运行在：

- 1 核 CPU。
- 2GB 内存。
- 几 GB 磁盘空间。
- 普通 Linux 云服务器。

资源控制原则：

- 单进程运行。
- SQLite 持久化。
- 不缓存大量连接数据。
- 每个 TCP 连接使用有限大小的缓冲区。
- 连接结束后及时释放资源。
- 日志按大小轮转。
- 不记录每一条 TCP 数据内容。
- 数据库定期清理过期操作日志。

## 12. V1.0 开发计划

**第一阶段：核心隧道**

- Server 启动。
- Agent 连接。
- Token 认证。
- 心跳机制。
- 端口申请。
- TCP 双向转发。
- 自动重连。
- 映射下线。

**第二阶段：管理 API**

- 创建映射 API。
- 查询映射 API。
- 下线映射 API。
- Agent 列表 API。
- SQLite 持久化。

**第三阶段：Web 页面**

- 登录页面。
- Dashboard。
- 映射列表。
- 创建映射。
- 下线映射。
- Agent 列表。
- 状态轮询。

**第四阶段：稳定性**

- 异常断线测试。
- 服务端重启恢复。
- 多连接并发测试。
- 端口冲突测试。
- 内存使用测试。
- 长时间运行测试。

## 13. V1.0 验收标准

1. 可以在公网服务器一键启动 Server。
2. 可以在本地机器启动 Agent。
3. Agent 可以通过 Token 认证接入。
4. Agent 可以申请自动分配的公网端口。
5. 支持指定公网端口。
6. 外部可以通过公网 IP 和端口访问本地服务。
7. 一个 Agent 可以创建多个映射。
8. 多个外部连接可以同时访问同一个映射。
9. Agent 断线后能够自动恢复。
10. 可以通过 API 创建和下线映射。
11. 可以通过 Web 页面查看和下线映射。
12. 服务端重启后可以恢复有效映射。
13. 整个服务端不依赖任何外部数据库或中间件。

---

**产品原则：简单、轻量、可靠、易部署。**

V1.0 只做好 TCP 端口映射这件事，不追求成为一个功能庞大的内网穿透平台。