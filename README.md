<div align="center">
  <h1>AirGate Anthropic</h1>

  <p><strong>Claude Messages API 网关插件</strong></p>

  <p>
    <img src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go" alt="go" />
    <img src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react" alt="react" />
  </p>
</div>

---

AirGate Anthropic 是 [airgate-core](https://github.com/DouDOU-start/airgate-core) 的 Claude 网关插件，基于 [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk) 构建。它以 gRPC 子进程方式运行，负责将 Claude Messages API 请求转发到 Anthropic 上游，支持多种认证方式和连接池复用。

## 核心特性

- **四种账号类型** — `apikey`（标准 API Key）、`oauth`（完整 scope 浏览器授权）、`setup_token`（仅推理 scope，1 年有效期）、`session_key`（claude.ai Cookie 自动换取 OAuth Token）
- **全类型自定义 Base URL** — 所有账号类型均支持自定义 API 地址，适用于反向代理或企业私有部署
- **进程内 Token 刷新锁** — 基于 sync.Mutex 的 per-account 并发保护，double-check 防止重复刷新，不可重试错误自动分类 + 60 秒冷却窗口
- **TLS 指纹伪装** — OAuth/session_key 账号使用 uTLS 模拟 Claude CLI 2.x 的 JA3 指纹，API Key 账号使用标准 TLS
- **连接池复用** — 标准 TLS 和 uTLS 指纹各自独立连接池，按 proxyURL 分桶缓存 Transport，避免每次请求创建新连接
- **精确成本计算** — 基于 SDK 的 `CalculateCost()` 填充 InputCost/OutputCost/CachedInputCost，支持 standard/priority/flex 计费层级
- **流式 Usage 提取** — SSE 流中实时提取 input_tokens、output_tokens、cache_read_input_tokens、reasoning_output_tokens，记录 FirstTokenMs
- **Console 批量导入** — 支持通过 Session Key 批量一键创建 OAuth/Setup Token 账号
- **完整前端 Widget** — 四种账号类型的表单面板，OAuth 授权引导、Session Key 一键获取、状态展示

## 接入位置

```text
                  +--------------------------------------+
                  |           AirGate Core               |
                  |   (账号 / 调度 / 计费 / 管理后台)     |
                  +----------------+---------------------+
                                   | go-plugin (gRPC)
                                   v
                  +--------------------------------------+
                  |    airgate-anthropic (本仓库)         |
                  |                                      |
                  |   apikey -----> api.anthropic.com    |
                  |   oauth  -----> api.anthropic.com    |
                  |   setup_token -> api.anthropic.com   |
                  |   session_key -> claude.ai (exchange) |
                  |                  -> api.anthropic.com |
                  +--------------------------------------+
```

**请求生命周期**：

```text
客户端请求 --> Core 鉴权 --> Core 选账号 --> Plugin.Forward()
                                                |
                                     +----------+-----------+
                                     v                      v
                              forwardAPIKey()        forwardOAuth()
                              (标准 TLS)          (uTLS 指纹 + token 刷新)
                                     |                      |
                                     v                      v
                              resolveBaseURL() -----> 上游 API
                                                        |
                                                        v
                                                  ForwardResult
                                            +-------+--------+
                                        token 用量      账号状态反馈
                                        fillCost()     Core 更新账号
```

## 路由

由 `metadata.go` 声明，core 启动时自动注册到网关：

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/messages` | Claude Messages API |
| POST | `/v1/messages/count_tokens` | Token 计数 |
| GET  | `/v1/models` | 模型列表（硬编码） |

## 账号类型

| Key | 标签 | 凭证字段 | 适用场景 |
|---|---|---|---|
| `apikey` | API Key | `api_key` + `base_url` | 标准 Anthropic API Key |
| `oauth` | OAuth 令牌 | `access_token` / `refresh_token` / `expires_at` + `base_url` | 浏览器 PKCE 授权，完整 scope |
| `setup_token` | Setup Token | `access_token` / `refresh_token` / `expires_at` + `base_url` | 仅 `user:inference` scope，1 年有效期 |
| `session_key` | Session Key | `session_key`（自动换取 token）+ `base_url` | claude.ai Cookie 自动 OAuth |

### OAuth 授权流程

**浏览器 PKCE 流程**：生成授权链接 -> 用户浏览器授权 -> 回调 URL 交换 Token

**Session Key 自动流程**：
1. `GET claude.ai/api/organizations` — 获取组织 UUID（优先选 team 类型）
2. `POST claude.ai/v1/oauth/{orgUUID}/authorize` — 获取授权码（PKCE）
3. `POST platform.claude.com/v1/oauth/token` — 交换 access_token + refresh_token

**Setup Token** 与标准 OAuth 的区别：scope 为 `user:inference`，请求时额外传 `expires_in: 31536000`（1 年）。

### Token 刷新机制

- 提前 3 分钟触发刷新（`tokenRefreshSkew`）
- 进程内 `sync.Mutex` per-account 锁，防止并发刷新
- 获锁后 double-check（其他 goroutine 可能已刷新）
- 不可重试错误（`invalid_grant`、`invalid_client`、`access_denied` 等）直接跳过，60 秒冷却
- 可重试错误最多 2 次指数退避重试
- 刷新失败不阻断请求，使用现有 token 继续转发

## 插件自定义端点

通过 `HandleRequest` 暴露给 Core，Core 透传到插件：

| 端点 | 说明 |
|------|------|
| `oauth/start` | 生成 OAuth 授权链接（PKCE） |
| `oauth/exchange` | 交换 code/session_key 获取 token，支持 `scope: "inference"` 创建 Setup Token |
| `oauth/refresh` | 手动刷新 token |
| `console/cookie-auth` | Session Key 一键创建 OAuth/Setup Token 账号 |
| `console/batch-cookie-auth` | 批量 Session Key 导入 |
| `usage/accounts` | 批量查询账号 5h/7d 用量 |

## 模型

| 模型 ID | 上下文 | 最大输出 | 输入价格 | 缓存价格 | 输出价格 |
|---------|--------|---------|---------|---------|---------|
| claude-opus-4-6 | 200K | 32K | $15.0 | $1.5 | $75.0 |
| claude-opus-4-5-20251101 | 200K | 32K | $15.0 | $1.5 | $75.0 |
| claude-sonnet-4-6 | 200K | 64K | $3.0 | $0.3 | $15.0 |
| claude-sonnet-4-5-20250929 | 200K | 64K | $3.0 | $0.3 | $15.0 |
| claude-haiku-4-5-20251001 | 200K | 8K | $0.8 | $0.08 | $4.0 |

价格单位：美元 / 百万 token。短名称自动规范化（如 `claude-sonnet-4-5` -> `claude-sonnet-4-5-20250929`）。

## 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.25 · gRPC · gjson · uTLS（JA3 指纹） |
| 前端 | React 19 · Vite · TypeScript（账号表单 Widget） |
| 插件协议 | hashicorp/go-plugin (gRPC) |
| 上游协议 | Anthropic Messages API · Anthropic OAuth |

## 安装与开发

### 方式 1：安装到 core（推荐）

打开 core 管理后台 -> **插件管理** -> 三种方式任选：

```text
1. 插件市场 -> 点击「安装」
2. 上传安装 -> 拖入二进制文件
3. GitHub 安装 -> 输入仓库地址
```

### 方式 2：源码运行（开发）

需要 Go 1.25+、Node 22+，以及兄弟目录 `airgate-sdk` 与 `airgate-core`：

```bash
git clone https://github.com/DouDOU-start/airgate-sdk.git
git clone https://github.com/DouDOU-start/airgate-core.git
git clone https://github.com/DouDOU-start/airgate-anthropic.git
cd airgate-anthropic
```

把本插件以 dev 模式挂到 core：

```yaml
# airgate-core/backend/config.yaml
plugins:
  dev:
    - name: gateway-anthropic
      path: /absolute/path/to/airgate-anthropic/backend
```

然后 `cd airgate-core/backend && go run ./cmd/server`，core 会自动启动本插件。

### 方式 3：不依赖 core 的端到端调试

```bash
cd backend && go run ./cmd/devserver   # 启动本地 devserver（模拟 core）
```

DevServer 提供完整的 OAuth/Console 端点，可独立测试授权流程和请求转发。

## 项目结构

```text
airgate-anthropic/
├── backend/
│   ├── main.go                            # gRPC 插件入口
│   ├── cmd/
│   │   ├── devserver/main.go              # 开发服务器（模拟 core）
│   │   └── genmanifest/main.go            # plugin.yaml 生成器
│   └── internal/gateway/
│       ├── gateway.go                     # GatewayPlugin 接口实现 + HandleRequest 端点
│       ├── metadata.go                    # 插件元信息（账号类型、路由、Widget）
│       ├── forward.go                     # 转发分发：apikey / oauth / session_key / setup_token
│       ├── stream.go                      # SSE 流式响应透传 + usage 提取
│       ├── models.go                      # 模型注册表 + LookupModel + fillCost
│       ├── headers.go                     # 认证头、Beta Header、Claude Code 伪装头
│       ├── errors.go                      # 错误分类、AccountStatus 推断、JSON 错误提取
│       ├── oauth.go                       # OAuth PKCE 流程、Session Key 交换、Token 刷新
│       ├── oauth_handler.go               # DevServer 用 OAuth/Console HTTP handler
│       ├── token_manager.go               # 进程内 Token 刷新锁（mutex + double-check + 重试）
│       ├── transport_pool.go              # HTTP Transport 连接池（标准 + uTLS 指纹）
│       ├── tlsfingerprint.go              # uTLS JA3 指纹配置（模拟 Claude CLI 2.x）
│       └── assets.go                      # WebAssetsProvider，embed webdist
├── web/                                   # 前端（账号表单 Widget）
│   └── src/components/AccountForm.tsx     # 四种账号类型表单 + OAuth 引导面板
├── plugin.yaml                            # genmanifest 自动生成
└── README.md
```

## 设计要点

- **`metadata.go` 是运行时真相** — 账号类型、路由、模型列表全从此派生，`plugin.yaml` 仅为分发产物
- **Token 刷新安全** — per-account mutex + double-check + 不可重试错误分类（移植自 sub2api 的 `OAuthRefreshAPI` 模式）
- **连接池分桶** — StandardTransportPool（API Key）和 FingerprintTransportPool（OAuth）各自独立，按 proxyURL 缓存，Init 创建 Stop 清理
- **统一 Base URL** — 所有账号类型通过 `resolveBaseURL()` 统一解析 `base_url` credential，消除硬编码
- **成本计算在插件内完成** — `fillCost()` 调用 SDK 的 `CalculateCost()`，Core 只做倍率乘法不关心模型定价
- **组织选择逻辑** — 多组织时优先选 `raven_type == "team"` 的组织（对齐 sub2api）

## 相关项目

- [airgate-core](https://github.com/DouDOU-start/airgate-core) — 核心网关（账号调度、计费、管理后台）
- [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk) — 插件 SDK
- [airgate-openai](https://github.com/DouDOU-start/airgate-openai) — OpenAI 网关插件（参考实现）
