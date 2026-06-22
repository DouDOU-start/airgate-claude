# airgate-claude — Claude 开发指南

> 叠加在 monorepo 根 `../CLAUDE.md` 之上。完整流程见共享 skill **`develop-plugin`**；接口契约见 `../airgate-sdk/CLAUDE.md`。

- **插件身份**：id `gateway-claude`，type `gateway`，上游 = Claude Messages API。
- 实现 `sdk.GatewayPlugin`：声明 models/routes/account fields，`Forward()` 转发并回 `ForwardOutcome`。

## 🚫 红线

通用边界铁律（只依赖 `airgate-sdk`、经 `Host.Invoke`/`InvokeStream` 调 core、`plugin.yaml` 由 `make manifest` 生成不可手改、前端单 `index.js` bundle）见 skill **`develop-plugin`「🚫 边界铁律」**。

## 混合现状（过渡态）

本仓当前混合了网关 + provider + UI 三层职责：

- **Provider 职责**（应归 provider 插件）：claude.ai OAuth（`oauth.go`/`oauth_handler.go`）、session-key 管理（`session.go`）、uTLS 指纹（`tlsfingerprint.go`）、sidecar（`sidecar.go`）、token 管理（`token_manager.go`）、传输池（`transport_pool.go`）
- **UI 职责**（应归 UI 插件）：6 个账号 widget

> 新增/改动须按职责归位，勿加深混合。详见 skill `core-dev`「技术债」。

## 命令

构建/发布命令见 skill **`develop-plugin`「构建 / 发布」**；本仓实际 make 目标以 `Makefile` 为准。
