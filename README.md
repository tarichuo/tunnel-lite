# Tunnel Lite

轻量 TCP 内网穿透工具：公网运行一个 Server，本地运行一个 Agent，通过 Web 管理映射。Go 单程序内嵌管理页面，使用 SQLite，无需外部数据库、Redis 或前端服务器。

已实现 TLS 控制与数据通道、独立 Agent Token、管理登录、端口分配、TCP 双向转发、并发限制、Agent 自动重连、Server 重启恢复，以及网页创建、查询、下线和再次启用映射。

## 编译

需要 Go 1.24+；产品运行时不需要 Node。SQLite 使用纯 Go 驱动，可关闭 CGO。

```bash
make build
```

输出为 `bin/tunnel-server` 和 `bin/tunnel-agent`。也可直接使用 `go build -buildvcs=false -o bin/tunnel-server ./cmd/tunnel-server`。

跨平台编译 Agent 示例：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -o bin/tunnel-agent.exe ./cmd/tunnel-agent
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -buildvcs=false -o bin/tunnel-agent-macos-arm64 ./cmd/tunnel-agent
```

## 首次运行

### 1. 初始化公网服务器

将下面的 `1.2.3.4` 替换为客户端实际连接的服务器公网 IP。本机体验可使用 `127.0.0.1`。

```bash
./bin/tunnel-server -init -dir ./runtime -public-host 1.2.3.4
./bin/tunnel-server -c ./runtime/server.yaml
```

初始化生成随机管理 Token、服务端证书、私钥、自建 CA 公共证书以及配置文件，不覆盖已有文件。配置与私钥默认权限为 0600，目录为 0700。CA 签名私钥不落盘。

管理端默认只监听 `127.0.0.1:7000`，控制与数据 TLS 入口为 `:7001`。映射端口池默认 `10000—20000`，可在启动前缩小范围。配置中的相对文件路径均以配置文件所在目录解析。

### 2. 打开管理端

本机体验访问 `http://127.0.0.1:7000`。公网部署时，可从管理员电脑使用 SSH 转发连接默认管理入口：

```bash
ssh -N -L 7000:127.0.0.1:7000 user@1.2.3.4
```

随后访问本机 `http://127.0.0.1:7000`，使用服务器 `runtime/server.yaml` 中的 `server.admin_token` 登录。页面不会保存这个 Token，登录后使用有期限的 HttpOnly 会话 Cookie。

需要直接通过 HTTPS 访问时，将 `api_host` 设为 `0.0.0.0`，并设置 `api_https: true`，访问 `https://公网IP:7000`。默认自建 CA 需由浏览器信任，也可将 `tls_cert`、`tls_key` 指向已有的受信任证书。明文 HTTP 模式仅允许绑定回环地址。当前原生会话校验使用实际请求 scheme/host，不信任 `X-Forwarded-*`；如使用反向代理，需确保该行为与浏览器入口一致，优先使用 TLS 到上游。

### 3. 接入本地 Agent

1. 网页点击“添加 Agent”，填写机器名称和允许转发的目标 IP，默认仅允许 `127.0.0.1`。
2. 下载 `agent.yaml` 和 `ca.crt`。Token 只在创建时展示一次，后续无法查询。
3. 将对应平台的 Agent 二进制与两个文件放在本地同一目录，运行：

```bash
./tunnel-agent -c agent.yaml
```

Agent 无需开放入站端口。它通过出站 TLS 主动连接 Server。控制和数据连接共用服务器 7001 入口，不需要额外的数据端口。

Server 需要能接收控制端口及实际使用的映射端口的外部访问；防火墙或云安全组的开放范围需与配置一致。

### 4. 创建映射

本地启动 TCP 服务，例如：

```bash
python3 -m http.server 3000 --bind 127.0.0.1
```

在网页选择在线 Agent，填写名称、本地 IP `127.0.0.1`、端口 `3000`，选择自动分配或指定公网端口。状态变为“运行中”后复制公网地址。对于这个 HTTP 示例，可用浏览器访问 `http://公网IP:分配端口`。

“运行中”表示 Agent 在线、配置确认且公网监听就绪；目标服务拒绝连接时，映射详情会显示最近转发失败提示。对外客户端到公网端口之间使用目标服务自己的协议；Tunnel Lite 的 TLS 保护覆盖 Server 与 Agent 之间。

## 映射生命周期

- Agent 临时离线：保留公网端口及启用配置，对新增连接快速关闭。重连后恢复原端口。
- Server 重启：从 SQLite 重建启用配置，等待 Agent 重连后恢复。端口被其他进程占用时显示错误，不擅自改端口。
- 主动下线：立即断开该映射的现有连接，关闭监听并释放端口；记录保留。
- 再次启用：自动分配模式优先使用原端口，占用时分配新端口并提示；指定端口模式冲突时报错。
- 创建或再次启用等待确认超过 10 秒：失败并回滚新监听；恢复已有映射失败时保留逻辑预留。
- 服务端与 Agent 均检查目标 IP 授权。修改本地 `allowed_targets` 后重启 Agent，不能通过网页扩大已有 Agent 进程的本地授权。

## API

基础地址：`http://127.0.0.1:7000/api/v1`。API 客户端使用 `Authorization: Bearer <管理 Token>`；浏览器使用 Cookie 并在写操作中附带 `X-CSRF-Token`。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST / GET / DELETE | `/session` | 登录、查询会话、退出 |
| GET | `/meta` | 公网展示地址、端口池、配额与 CA 公共证书 |
| GET | `/overview` | 全局统计 |
| POST / GET | `/agents` | 签发 Agent 身份 / Agent 列表 |
| GET | `/agents/{id}` | Agent 及最近操作 |
| POST / GET | `/tunnels` | 创建 / 分页查询映射 |
| GET | `/tunnels/{id}` | 映射及最近操作 |
| DELETE | `/tunnels/{id}` | 幂等软下线 |
| POST | `/tunnels/{id}/start` | 再次启用 |

创建 Agent：

```json
{"name":"dev-machine","allowed_targets":["127.0.0.1"]}
```

返回 `201` 和 `{ "agent": {...}, "token": "仅本次返回" }`。

创建映射：

```json
{"agent_id":"agent_...","name":"web","local_address":"127.0.0.1","local_port":3000,"remote_port":0}
```

返回 `202` 与 `STARTING` 的映射对象。使用详情或列表查询最终状态。`remote_port: 0` 表示自动分配，响应使用 `public_port`。请求可带 `Idempotency-Key`，同进程内保留 24 小时，相同键和内容返回原记录，不同内容返回冲突。该缓存不跨 Server 重启；同一 Agent 的映射名称也有唯一性检查。

再次启用的请求为 `{"remote_port":0}`（自动）或 `{"remote_port":18080}`（指定）。公网端口被占用时返回 `409`。指定模式下调用方应发送希望保留的原端口，网页已自动预填。

列表查询参数：`q`、`agent_id`、`status`、`page`、`page_size`（1—100，默认 20）。不指定 status 时隐藏 `STOPPED`，`status=ALL` 显示全部。返回 `{items,total,page,page_size}`。详情返回 `{tunnel,operations}` 或 `{agent,operations}`。

错误统一返回：

```json
{"error":{"code":"PORT_IN_USE","message":"该公网端口已被占用，请更换或使用自动分配","field":"remote_port","request_id":"..."}}
```

## 运维与限制

示例配置与 systemd 单元位于 [examples](examples/)。systemd 示例使用专用 `tunnel` 用户，需自行创建该用户，并将二进制及配置安装到示例路径、设置所属用户后启用。

默认每 Agent 最多 20 个有效映射、每映射最多 64 个连接、Server 最多 256 个已接受转发连接。每个连接方向使用 32KiB 缓冲区，无无限排队。待数据配对默认 10 秒超时，未认证入口限制为 128 个并行握手，帧大小不超过 64KiB。

Server 日志写入 stderr 与 `logs/tunnel.log`，默认单文件 5MiB、保留 3 个备份。操作记录每分钟清理，保留最近 30 天且最多 10000 条；清理周期内可暂时超过数量上限。不记录 TCP 载荷和完整 Token。Agent 日志写入 stderr，作为系统服务运行时使用宿主日志系统管理保留期限。

SQLite 文件、WAL/SHM 可能同时存在。备份时停止 Server 后复制整个 data 目录，连同服务端 YAML、私钥和 CA 妥善保存。不要在数据库运行期间仅复制主文件。

默认服务端证书有效期为 1 年、CA 为 5 年，暂不提供 ACME 或自动续期。使用自建证书轮换时，在新目录重新初始化，将新的 CA 同步至所有 Agent，并更新服务端配置；管理 Token 的变更也需同步到部署配置。V1.0 尚无 Agent 删除、Token 重置与多用户权限管理。

目标运行环境为 1 核 / 2GB Linux；实际并发能力仍需根据目标服务、带宽和连接生命周期压测。当前未完成生产环境长时间运行或容量基准验证。

## 验证

```bash
make test   # 真实 TCP 集成测试与 Go 竞态检查，需要允许监听本地临时端口
make check  # go vet 与前端 JavaScript 语法检查（需要 Node）
```

集成测试覆盖 TCP 二进制数据与半关闭、并发、Token/TLS/CSRF、Agent 本地授权、端口冲突、断线重连、Server 重启恢复、下线释放端口及再次启用。

可选浏览器测试使用系统 Chrome 和 Playwright Core；仅作为开发工具，不属于产品运行依赖：

```bash
npm install --prefix /tmp/tunnel-browser-tests --no-audit --no-fund --ignore-scripts playwright-core@1.64.1
PLAYWRIGHT_MODULE=/tmp/tunnel-browser-tests/node_modules/playwright-core/index.mjs node scripts/browser-smoke.mjs
```

可通过 `CHROME_PATH` 指定 Chrome 路径。脚本使用临时配置和端口，退出后关闭进程，并输出桌面端及移动端截图位置。

产品依据：[PRD](PRD.md)、[产品设计](docs/PRODUCT_DESIGN.md)。
