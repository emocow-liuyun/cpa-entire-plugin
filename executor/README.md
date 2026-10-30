# cpa-entire-plugin / executor

CLIProxyAPI v8 原生 **Executor** 动态库插件。把 `entire-review-*` 模型族接到 Entire 的 review
Agent 路线，并把整个执行路径做成 **fail-closed**：本版本任何真实派单都返回 `503`，不会返回伪造的成功响应。

- 只声明 `executor` 一项能力：`executor.identifier` / `executor.execute` / `executor.execute_stream` /
  `executor.count_tokens` / `executor.http_request`，外加模型与认证元数据：`model.register` / `model.static` /
  `auth.parse` / `auth.refresh`。
- **不声明 `quota_provider`**，也不实现 `quota.fetch` / `quota.reset`：额度读取由 `../quota` 负责，本模块
  不重复提供 quota provider。
- 不发起推理请求、不启动 Agent 任务、不改上游状态。本版本从不访问网络。

## 与其他子模块的边界

| 目录 | 职责 | 本模块的依赖方式 |
| --- | --- | --- |
| `../quota` | 额度（billing）读取，quota_provider 能力 | 无依赖，不调用、不复制其代码 |
| `../guard` | 纯 Go 花费准入内核（单账户串行、冷却、预留） | 无依赖；执行闸门的语义与它对齐，但本模块独立实现 |
| `../bridge` | Entire Agent 控制面客户端（run 启动/轮询/取消） | 无依赖；设计文档说明如何将其接入 `Dispatcher` 接缝 |

三个同级目录均未修改。

## 文件

| 文件 | 作用 |
| --- | --- |
| `abi.go` | C ABI v1 桥接：`cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`，`host.*` 回调封装 |
| `envelope.go` | 插件/宿主 JSON 信封、错误码、HTTP 状态透传、脱敏出口 |
| `plugin.go` | RPC 分发、注册元数据、配置生命周期、`Dispatcher` 接缝定义 |
| `config.go` | `config_yaml` 解析（yaml.v3）、默认安全值、**执行闸门判定** |
| `credential.go` | Bearer token 解析优先级、JWT `exp` 只读解码、auth 文件解析 |
| `guard.go` | 出站 URL 白名单、scheme、IP/userinfo/fragment 拦截 |
| `model.go` | `entire-review-*` 模型命名空间、思考后缀剥离、模型目录 |
| `request.go` | chat-completions 解析与**严格拒绝**（工具/图片/stream/不支持参数） |
| `execute.go` | `executor.execute` / `executor.count_tokens` 主流程、完成响应渲染、host 传输 |
| `auth.go` | `auth.parse` / `auth.refresh` |
| `redact.go` | 凭据值登记与全局脱敏 |
| `plugin_test.go` | 36 个用例（含 gate 矩阵、拒绝矩阵、seam、auth、脱敏） |
| `tools/abi_smoke.py` | ctypes 原生 ABI 冒烟测试（mock host + 合成 token，15 组断言） |
| `INTEGRATION.md` | 真实执行的独立整合方案（接口、步骤、风险与验收） |

## 构建与验证

```bash
cd /mnt/新加卷/DSH/default-workspace/cpa-entire-plugin/executor
./build.sh                     # go vet + go test + go test -race + c-shared + ABI 冒烟
```

产物：`libentire_executor.so`（插件 ID = 去掉 `lib` 前缀与 `.so` 的文件名 → `cpa-entire-executor`）。

## RPC 契约

宿主 → 插件（本模块实现）：

| 方法 | 请求 | 结果 |
| --- | --- | --- |
| `plugin.register` / `plugin.reconfigure` | `{config_yaml: base64, schema_version: 8}` | `{schema_version, metadata, capabilities}` |
| `plugin.quiesce` / `plugin.shutdown` | `{}` | `{}` |
| `executor.identifier` | `{}` | `{"identifier":"entire-agent"}` |
| `model.register` / `model.static` | `{Plugin:{...}}` | `{Provider:"entire-agent", Models:[...]}` |
| `executor.execute` | `pluginapi.ExecutorRequest` + `host_callback_id` | `pluginapi.ExecutorResponse` |
| `executor.execute_stream` | 同上 + `stream_id` | **拒绝**：`entire_stream_unsupported` / HTTP 400 |
| `executor.count_tokens` | `pluginapi.ExecutorRequest` + `host_callback_id` | `{"total_tokens": N}`（本地估算，不联网） |
| `executor.http_request` | `pluginapi.ExecutorHTTPRequest` | **拒绝**：`entire_real_execution_not_implemented` |
| `auth.parse` | `pluginapi.AuthParseRequest` | `pluginapi.AuthParseResponse` |
| `auth.refresh` | `pluginapi.AuthRefreshRequest` | 原样回传，`entire_refresh=unsupported` |

插件 → 宿主（本模块只调用这两条）：

| 方法 | 请求 | 结果 |
| --- | --- | --- |
| `host.log` | `{level, message, fields}` | `{}` |
| `host.http.do` | `{host_callback_id, method, url, headers, body}` | `{StatusCode, Headers, Body}` |

`host.http.do` 只出现在未来接入 `Dispatcher` 时使用的 `hostTransport` 中；本版本没有 dispatcher，
因此该路径不会被走到（ABI 冒烟会断言 `host.log` 之外零调用）。

### 关键 ABI 细节（已按 SDK 对齐）

1. `abi_version` = 1，与 `pluginabi.ABIVersion` 一致。
2. `pluginapi` 结构体**没有** JSON tag，序列化后的字段名就是 Go 字段名（`Model` / `Payload` / `Stream` /
   `HostCallbackID` / `StatusCode` …）。本模块的请求结构显式复刻这些名字。
3. Go `[]byte` 在 JSON 里是 **base64**：`config_yaml`、`Payload`、`OriginalRequest`、`StorageJSON`、
   `RawJSON`、`HostHTTPResponse.Body` 全部按 base64 编解码。
4. 失败信封必须带 `http_status`，否则宿主默认落到 500。本模块的每个拒绝都显式给 400/404/503/502。
5. `executor.execute_stream` 在宿主侧会先 `open(stream)` 并下发 `stream_id`。本模块拒绝该方法而不是
   返回空 chunk 通道，避免宿主拿到一个“成功但没有内容”的流。

## 配置

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-entire-executor:          # 插件 ID = 动态库文件名去掉 lib 前缀与 .so
      enabled: true
      priority: 1
      token: ""                   # 账户持有人自己提供的 Entire Bearer token
      token-env: "ENTIRE_TOKEN"
      core-base-url: "https://us.console.entire.io"
      cell-base-url: "https://aws-us-east-2.api.entire.io"
      allow-domains: ["entire.io", "console.entire.io", "us.console.entire.io", "api.entire.io"]
      project-host: "gh"
      project: "acme/widgets"
      agent-id: ""
      trail-id: ""
      change-id: ""
      # ---- 执行闸门，全部默认 false ----
      execution-enabled: false
      auth-subject-verified: false
      billing-limit-enforced: false
      allow-unbounded-spend: false
```

## 执行闸门（fail-closed 的全部内容）

`executor.execute` 依次检查，任一不过即拒绝，**不会**触碰上游：

| # | 条件 | 失败时 |
| --- | --- | --- |
| 1 | 模型在 `entire-review-*` 命名空间内 | `entire_model_unsupported` / 404 或 400 |
| 2 | 载荷是合法 chat-completions JSON | `entire_payload_invalid` / 400 |
| 3 | 无工具、无图片、无 `stream: true`、无非文本内容 | `entire_tools_unsupported` / `entire_image_unsupported` / `entire_stream_unsupported` / `entire_content_unsupported`，均 400 |
| 4 | 无不支持的参数（`response_format`、`n>1`、`logprobs`、`reasoning_effort`…） | `entire_params_unsupported` / 400 |
| 5 | `execution-enabled: true` | `entire_execution_disabled` / 503 |
| 6 | `auth-subject-verified: true` | `entire_execution_insecure_transport` / 503 |
| 7 | `billing-limit-enforced: true` | `entire_billing_limit_unavailable` / 503 |
| 8 | `allow-unbounded-spend: true` | `entire_billing_limit_unavailable` / 503 |
| 9 | 存在可用 Bearer token | `entire_credential_missing` / 503 |
| 10 | run 目标齐全（project-host/project/agent-id/trail-id/change-id） | `entire_execution_disabled` / 503 |
| 11 | 已安装真实 `Dispatcher` | `entire_real_execution_not_implemented` / **503** |

### 为什么第 8 和第 11 条存在

Entire 的 Agent 运行按用量计费，但控制面**不提供硬性的单次费用上限**：一次 run 可以在客户端拿到
响应前消耗任意信用。`billing-limit-enforced` 只能表达“运维承诺有预算”，无法表达“上游能拦住超额”。

所以：

- 第 8 条要求运维**显式确认**自己接受“无上限花费”这件事。它不是默认值，插件也不会替运维打开。
- 即使全部条件满足，本版本**没有安装 dispatcher**，第 11 条仍会拦住，返回 `503`
  与 `entire_real_execution_not_implemented`。

这是刻意的：让“网关返回了内容，所以扣费一定被控制住了”这种误解不可能发生。真实执行的设计见
[INTEGRATION.md](INTEGRATION.md)。

`auth.parse` 与 `executor.count_tokens` 不联网，不消耗信用，所以不受上述闸门约束。

## 严格拒绝清单

以下请求一律 `400`，不做静默丢字段：

- `tools` / `tool_choice` / `parallel_tool_calls: true` / 旧版 `functions` / `function_call`
- `messages[]` 中 `role: tool|function`、`tool_calls`、`function_call`、`tool_call_id`
- 任何 `image_url` / `input_image` / `image` 内容片段；`modalities` 含非 `text`；`audio`
- `input_file` / `file` / `document` / `input_audio` / 其他未知 content type
- `content` 的 text 片段携带 `type`/`text` 以外的字段（防止夹带图片引用）
- `stream: true`（载荷或路由标志任一出现）
- `response_format.type != "text"`、`n != 1`、`logprobs: true`、`top_logprobs`、`reasoning_effort`
- 空 body、非 JSON、JSON 后带尾随数据、`messages` 为空

透明放行：`n: 1`、`logprobs: false`、`response_format: {"type":"text"}`、`parallel_tool_calls: false`、
纯字符串 `content`、只含 `type=text` 的内容片段数组、以及宿主追加的思考后缀（`entire-review-review(high)`）。

## 模型

| 模型 | 映射 harness | 说明 |
| --- | --- | --- |
| `entire-review-review` | `trail-review` | 主线评审 |
| `entire-review-review-fast` | `trail-review` | 预留别名 |
| `entire-review-review-deep` | `trail-review` | 预留别名 |
| `entire-review-review-light` | `trail-review` | 预留别名 |

`model.register` 会把这些名字发布给宿主；由于声明了 Executor，宿主会把它绑定到本 provider。

## 凭据与脱敏

优先级：`token` 配置 → `token-env` 环境变量 → auth `StorageJSON`（`entire_token` / `access_token` /
`token` / `bearer_token`）→ auth `Metadata`。

- `normalizeToken` 去掉 `Bearer ` 前缀与首尾空白，拒绝含控制字符或内嵌空格的值。
- 每个成功解析出的 token 都会登记到全局脱敏表；所有出站消息（错误、host.log）在序列化前统一扫描替换。
- `describeConfig` 只报告 `token=set|unset`，从不打印值。
- 冒烟测试断言：15 组响应体和全部 host.log 记录中都不出现合成 token。

## 已知限制

- 本版本不执行任何真实派单。这是设计目标，不是缺口。
- `executor.execute_stream` 未实现：Entire 的 run 结束才有完整文本，没有可流式的 token 通道。
- 客户端工具调用没有回传通道：云端 Agent 自己跑工具，无法把 `tool_calls` 交回客户端。
- `auth.refresh` 是空操作：Bearer 是长期 token，没有可用的 refresh 端点。
