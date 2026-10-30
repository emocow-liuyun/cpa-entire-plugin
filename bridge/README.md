# bridge — Entire Agent 任务桥接客户端（Go 标准库）

`bridge/` 是 `cpa-entire-plugin` 的 Entire Agent 任务桥接层。它封装一条窄链路：读账单、读/写 Agent 配置、启动 Run、轮询到终态、提取文本、上下文取消时尽力 cancel。**它不是模型网关。**

## 它做什么

| 调用 | 路由 | 关键约束 |
|---|---|---|
| `GetBilling(ctx, refresh)` | `GET {core}/me/billing` | 只取 `state/trial/balance/overage/runs_paused/unavailable`，不取支付方式 |
| `GetBillingSpend(ctx, period)` | `GET {core}/me/billing/spend?period=YYYY-MM` | 按月花费 |
| `ListAgentModels(ctx)` | `GET {cell}/agents/models` | 模型目录，`FindAgentModel` 校验 harness/model/effort |
| `ReadAgent(ctx, id)` | `GET {cell}/{host}/{project}/agents/{id}` | 记录 JSON `etag`（优先于 HTTP ETag 头） |
| `ReplaceAgent(ctx, id, ifMatch, body)` | `PUT {cell}/{host}/{project}/agents/{id}` | **必须带 `If-Match`**；本地先校验 16384 上限 |
| `StartRun(ctx, opts)` | `POST {cell}/{host}/{project}/agents/{id}/runs` | **必须带 `Idempotency-Key`**，body 为 `RunCreate{target,head_sha}` |
| `PollRun(ctx, runID, opts)` | `GET {cell}/{host}/{project}/runs/{id}` | 轮询到 `succeeded/failed/timed_out/canceled` |
| `CancelRun(ctx, runID)` | `POST {cell}/{host}/{project}/runs/{id}/cancel` | 取消只对 queued/running 有效 |
| `Run(ctx, opts)` | 组合 Start + Poll | 不确定的 start 不自动重试 |

## 安全与凭据

- **bearer 只从配置注入**：`Config.Bearer`（YAML `token`），或 `Config.BearerEnv` 指名的环境变量。构造后被移入 `Client` 私有字段，`Client.Config()` 返回的副本 `Bearer` 恒为空。
- **任何输出都不打印 bearer**：`Config.String()`/`GoString()` 只输出 `token=set|unset`；错误路径的响应体经 `redactErrorBody` 把 `Bearer xxxx` / `Authorization: xxxx` 替换为 `[redacted]`；`*HTTPError`、`*DecodeError`、`*PreflightError` 全部走这条路径。
- **固定域白名单**：`core_base_url` + `cell_base_url` 的 `host:port` 自动入白名单，另可加 `allowed_hosts`（不带端口＝该主机任意端口）。每次拼出 URL 后调用 `checkOrigin`，不在白名单直接返回 `ErrOriginNotAllowed`，请求不发。空白名单不是通配。
- **不跟随重定向**：`CheckRedirect` 返回 `http.ErrUseLastResponse`，30x 作为 `*HTTPError` 返回，bearer 不会被带去别的域。
- **路径注入防御**：id 走 `escapeSegment`（比 `url.PathEscape` 更严，`.`/`..` 也编码），`#` 拒绝，`?` 只在计费接口由代码显式拼接。
- **httptest 注入**：`New(cfg, transport http.RoundTripper)` 接受任意 transport；测试传 `httptest.Server.Client().Transport`，生产传宿主的有日志 transport。`SetTransport` 可在构造后替换。`AllowHTTP` 仅用于 127.0.0.1 的 httptest。

## 错误语义

```go
ErrMissingBearer / ErrInsecureEndpoint / ErrOriginNotAllowed / ErrNotConfigured / ErrInvalidConfig
ErrNotFound(404) / ErrConflict(409) / ErrPreconditionFailed(412) / ErrPreconditionRequired(428)
ErrRunFailed / ErrRunCanceled / ErrOutputEmpty / ErrOutputTruncated
ErrContextCanceled / ErrPollTimeout
```

全部可用 `errors.Is` 判定；`*HTTPError` 同时可被 `errors.As` 取出并带有 `Problem{code,detail,request_id}`。

## 文本提取（`ExtractText`）

顺序：`result.signal.rationale`（`review_changes_result.signal.rationale` 或 `Run.result.data.signal.rationale`）→ `review_changes_result.summary` → `output.text`。

- `output.truncated == true`：返回已取到的文本，**同时**报 `ErrOutputTruncated`，不把前缀当完整答案。
- `succeeded` 但三处都空：`ErrOutputEmpty`。
- `failed`/`timed_out`：`ErrRunFailed`，附带 `error.code`、`error.message`、脱敏后的 `diagnostic`。
- `canceled`：`ErrRunCanceled`。

## 取消

`PollRun` 在 ctx 结束或本地 `PollTimeout` 到期且 Run 未终态时，用 `context.WithoutCancel(ctx)` + `RequestTimeout` 的独立超时上下文尽力 `POST .../cancel`：成功则在错误里标注 `cancel requested (status now ...)`，失败标注 `cancel also failed: ...`，原始原因始终保留。`PollOptions.CancelOnAbort` 置 false 可关闭。

## 明确未实现（不得宣称完全兼容）

- **没有真正的 SSE**。Run 文本是终态后一次性读取；`PollRun` 每轮回调 `OnStatus` 只报告 `queued/running/...`，不模拟 token 流。
- **没有工具调用**。云端 Agent 自己跑工具，不向客户端回传 `tool_calls`，也没有客户端工具回路。
- **不是 Chat Completions / Responses / Messages 接口**。`RunCreate` 只接受 `target{trail_id,change_id}` 与可选 `head_sha`，没有 messages/input/tools/stream。
- 没有 `agents` 创建/归档/暂停/版本回滚，没有 environment/trail/change 管理，没有 discussion stream。
- 长输出已截断时无法取回全文（无全文路由）。

## 测试

```bash
cd bridge && go test ./...          # 76 个用例，覆盖率 89.3%
cd bridge && go test -race ./...    # 无竞态
```

全部用例只连 `httptest` 的 127.0.0.1，不触真实账户、不发付费请求、不启动 Run。覆盖点：bearer 不进 body/日志/错误、白名单拒绝不发请求、不跟随重定向、If-Match 必填与 428/412 分类、16384 上限本地拦截（含多字节）、Idempotency-Key 生成与 128 上限、终态提取优先级、截断/空输出/失败/取消错误、ctx 取消触发 cancel 且 cancel 失败也上报、结果 data 与 mirror 两种 rationale 读法。

## 配置示例

```yaml
enabled: true
core_base_url: "https://us.console.entire.io"
cell_base_url: "https://aws-us-east-2.api.entire.io"
project_host: "gh"
project: "acme-widgets"
agent_id: "01HZZ..."
trail_id: "01HZZ..."
change_id: "01HZZ..."
token_env: "ENTIRE_TOKEN"   # 或 token: "..."（不写进日志）
poll_interval: "10s"
poll_timeout: "30m"
```

用法：

```go
client, err := bridge.New(cfg, hostTransport)
agent, err := client.ReadAgent(ctx, "")
repl := bridge.AgentFrom(agent)          // 全量替换：不丢字段
repl.Instructions = prompt               // 超 16384 字符会在本地 ErrInvalidConfig
_, err = client.ReplaceAgent(ctx, "", "", repl)   // If-Match 用 ReadAgent 缓存的 etag

res := client.Run(ctx, bridge.RunOptions{
    StartRunOptions: bridge.StartRunOptions{IdempotencyKey: key}, // 重试用同一个 key
    PollOptions:     bridge.PollOptions{Interval: 10 * time.Second},
})
if res.Err != nil { ... }                // 失败/截断/取消都有明确错误
fmt.Println(res.Source, res.Text)
```
