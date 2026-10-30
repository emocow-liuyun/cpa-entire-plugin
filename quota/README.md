# cpa-entire-plugin / quota

CLIProxyAPI v8（`github.com/router-for-me/CLIProxyAPI/v8`）原生 **QuotaProvider 动态库插件**，把 Entire 的
预付额度（credit）读取结果映射为管理端可渲染的归一化配额结构。

- 只声明 `quota_provider` 一项能力：`quota.identifier` / `quota.describe` / `quota.fetch` / `quota.reset`。
- 不注册 Executor、AuthProvider、ManagementAPI、请求/响应拦截等其他能力。
- 不发起推理请求、不启动 Agent 任务、不修改任何上游状态，只读 `GET .../billing`。

## 文件

| 文件 | 作用 |
| --- | --- |
| `abi.go` | C ABI v1 桥接：`cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`，以及 `host.*` 回调封装 |
| `envelope.go` | 插件/宿主 JSON 信封、错误码、脱敏出口 |
| `plugin.go` | RPC 分发、注册元数据、配置生命周期 |
| `config.go` | `config_yaml` 解析（yaml.v3）与安全默认值 |
| `credential.go` | Bearer token 解析优先级与 JWT `exp` 只读解码 |
| `guard.go` | 域名白名单、scheme、IP/userinfo/fragment 拦截 |
| `upstream.go` | `host.http.do` 传输、billing URL 构造、HTTP 状态映射 |
| `billing.go` | `BillingStatus` / `BillingBalance` / `BillingAutomaticTopUp` 等 schema 解码 |
| `quota.go` | 归一化映射与 fail-closed 规则 |
| `redact.go` | 凭据值登记与全局脱敏 |
| `plugin_test.go` | 20 个测试函数 + 12 个子用例（合计 32 条通过） |
| `tools/abi_smoke.py` | ctypes 原生 ABI 冒烟测试（mock host + 合成 token） |

## RPC 契约（来自 `sdk/pluginabi`、`sdk/pluginapi` 与 `internal/pluginhost`）

宿主导出给插件的函数：

```c
int  cliproxy_plugin_init(cliproxy_host_api* host, cliproxy_plugin_api* plugin);
int  cliproxyPluginCall(char* method, uint8_t* req, size_t req_len, cliproxy_buffer* resp);
void cliproxyPluginFree(void* ptr, size_t len);
void cliproxyPluginShutdown(void);
```

宿主 → 插件（本插件实现）：

| 方法 | 请求 | 结果 | 备注 |
| --- | --- | --- | --- |
| `plugin.register` | `{config_yaml: []byte(base64), schema_version: 8}` | `{schema_version, metadata, capabilities{quota_provider:true}}` | `internal/pluginhost/rpc_client.go:registerRPCPlugin` |
| `plugin.reconfigure` | 同上 | 同上 | 已注册后宿主改发 reconfigure |
| `plugin.quiesce` / `plugin.shutdown` | `{}` | `{}` | |
| `quota.identifier` | `{}` | `{"identifier":"entire"}` | `callPluginIdentifier`，注册时调用一次并缓存 |
| `quota.describe` | `{plugin:{...}}` | `{supported_providers, display_name, supports_reset:false}` | `internal/pluginhost/rpc_client.go:711` |
| `quota.fetch` | `QuotaFetchRequest` + `host_callback_id` | `QuotaFetchResponse` | `internal/pluginhost/rpc_client.go:717` |
| `quota.reset` | `QuotaResetRequest` + `host_callback_id` | `{success:false, message}` | `internal/pluginhost/rpc_client.go:726` |

插件 → 宿主（本插件只调用这两条）：

| 方法 | 请求 | 结果 |
| --- | --- | --- |
| `host.http.do` | `{host_callback_id, method, url, headers, body}` | `{StatusCode, Headers, Body}` |
| `host.log` | `{level, message, fields}` | `{}` |

关键细节（已在源码核对）：

1. `QuotaFetchRequest.HTTPClient` 在 RPC 路径上会被 `sanitizePluginRequest` 置为 nil，因此插件必须通过
   `host.http.do`（`host_callback_id` 由宿主在 `quota.fetch` 中下发）访问上游，而不是自建 `http.Client`。
2. `pluginapi.HTTPResponse` 没有 JSON tag，序列化后字段名就是 `StatusCode` / `Headers` / `Body`；Go 的
   `[]byte` 在 JSON 中是 **base64** 字符串。`config_yaml` 同理是 base64。
3. `host_callback_id` 在插件侧是不透明字符串（宿主生成递增 id），只做原样回传。
4. 宿主 `checkPluginRedirect` 在跨源跳转时会剥离凭据头；本插件另外只允许 2xx 结果，遇到 `3xx` 直接失败。

## 配置

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-entire-quota:        # 插件 ID = 动态库文件名（去掉 lib 前缀与 .so）
      enabled: true
      priority: 1
      base-url: "https://us.console.entire.io"
      allow-domains: ["entire.io", "console.entire.io", "us.console.entire.io", "api.entire.io"]
      require-top-up-on-failure: true
      timeout-seconds: 15
      # org-id: "01JABCDEFGHJKMNPQRSTVWXYZ"   # 可选：改读 /orgs/{org_id}/billing
```

`token` / `token-env` / `refresh-cache` 已删除：

- **token / token-env**：一个 CPA 安装可以同时挂多个 Entire 凭证，插件全局 token 会
  覆盖每个 credential 自己的 token。token 只从**每个 credential 自己的** auth storage
  （`entire_token` / `access_token` / `token` / `bearer_token`）或 auth metadata
  （`entire_token` / `token` / `bearer_token`）解析，没有全局回退。
- **refresh-cache**：对真实 `GET /api/v1/me/billing` 附加 `?refresh=all` 已实测返回 HTTP 400，
  该设置没有依据，默认关闭并已移除。

`allow-domains` 等配置会出现在注册元数据的 `ConfigFields` 里，供管理端渲染。

## 认证解析优先级（每凭证）

1. 该凭据的 auth storage JSON：`entire_token` / `access_token` / `token` / `bearer_token`
2. 该凭据的 auth metadata：`entire_token` / `token` / `bearer_token`

没有插件全局 token、没有环境变量回退：多 Entire 凭证安装下，全局 token 会覆盖各 credential
自己的 token，因此被刻意移除。

- 接受带或不带 `Bearer ` 前缀；含空白或控制字符的 token 视为无效。
- 只读取显式 bearer token 字段；OAuth 交互式登录、Cookie 会话一律不参与。
- 两层全部为空 → `entire_credential_missing`，**fail closed**，不发任何上游请求。
- token 一旦解析，立即进入脱敏登记表；错误消息、`host.log` 字段、ABI 信封出口都会被替换成 `[REDACTED]`。
  token 只出现在发往白名单主机的 `Authorization` 请求头里。登记表**没有 LRU 淘汰**：
  被淘汰的 token 之后会以明文出现在消息里，因此登记条目常驻进程生命周期。

## 域名白名单

- 默认：`entire.io`、`console.entire.io`、`us.console.entire.io`、`eu.console.entire.io`、`api.entire.io`
  （精确匹配或 `*.` 子域匹配）。
- 强制 `https`；`http` 仅对显式列入白名单的 loopback 主机（`localhost`/`127.0.0.1`/`::1`）放行，用于本地夹具。
- 拒绝：URL 内嵌 userinfo、带 fragment、非 IP 白名单的裸 IP 字面量、白名单外的任意主机。
- 请求路径固定为 `/api/v1/me/billing` 或 `/api/v1/orgs/{org_id}/billing`，不接受任意路径拼接。

## 归一化映射

`QuotaFetchResponse`：

- `subscription.plan` / `tierName`：`Prepaid credit`、`Trial credit`、`Billing pending card` 等，由 `state` + `trial.state` 推导。
- `groups[0] = "Entire credits"`，唯一桶：
  - `window: "total"` — `remainingFraction = 1`（余额真实可信时），`description` 写入余额与精确分值，
    如 `credit balance $123.45 (12345 cents)`；不可消费状态/零余额时 `remainingFraction = 0`。
- **没有 `budget` 桶、没有 `Spending limits` 组、没有任何 `min(余额, target)/target` 比例展示**：
  自动充值的 `target_cents` 是**充值目标**（充到多少钱），不是 spending limit，用它推导
  "可消费比例"是虚假语义，已全部删除。
- `summary`（管理端渲染的数值指标）：

| key | 说明 | format |
| --- | --- | --- |
| `entire_credit_balance` | 真实剩余总信用（`balance.total_cents`） | `currency` / USD |
| `entire_trial_remaining` | 试用信用余额（字段存在时） | `currency` |
| `entire_purchased_credit` | 购买信用余额（字段存在时） | `currency` |
| `entire_granted_credit` | 赠送信用余额（字段存在时） | `currency` |
| `entire_overage` | 本期超额使用 | `currency` |
| `entire_autotopup_enabled` | 自动充值启用状态（1=开 / 0=关） | `number` |
| `entire_runs_paused` | 计费暂停标记 | `number` |

`entire_spend_limit` / `entire_autotopup_threshold` / `entire_autotopup_target` 已删除：
阈值与目标是充值触发/充值目标配置，不是额度事实，管理端不应把它们渲染成配额。

## fail closed 规则（按顺序）

1. 缺 `state`，或 `state` 不是 `active` / `not_started` / `pending_card` / `provisioning`
   → `entire_quota_unknown`。
2. `not_started` / `pending_card` / `provisioning` → 明确给出 0 桶 + `entire_quota_unknown`（`remainingFraction: 0`，状态本身可信但不能消费）。
3. `unavailable` 含 `balance` / `overage` / `runs_paused`，或对应字段缺失 → `entire_billing_unreadable`。
4. `balance.total_cents < 0` → `entire_billing_unreadable`。
5. `runs_paused.paused = true` → 0 桶 + `entire_quota_unknown`（余额不可消费）。
6. **`balance.total_cents == 0` → 明确为零，`ok:true`**，`summary.entire_credit_balance = 0`，
   桶 `remainingFraction = 0`，描述中含 `0 cents`；不再当作 unknown。
7. 自动充值（`automatic_top_up`）：
   - `unavailable` 含 `automatic_top_up` 或 `automatic_top_up.last_charge_failed` → `entire_billing_unreadable`
     （`require-top-up-on-failure: false` 时降级为"无自动充值"，继续正常映射）。
   - `enabled` 缺失 → `entire_quota_unknown`。
   - `last_charge_failed: true` → `entire_quota_unknown`（同上可降级）。
   - `enabled: true` 但阈值/目标缺失或 `target < threshold` 或为负 → `entire_billing_unreadable`。
   - 校验通过后只输出 `entire_autotopup_enabled`（number），不输出阈值/目标数值。

插件内部错误标记：上游 `401` → `entire_unauthenticated`；`403` → `entire_forbidden`；管理 API 在部分宿主版本会统一返回 HTTP 502。宿主回调使用请求上下文，本插件的 `timeout-seconds` 尚未通过宿主回调强制执行。
`421` 及其他非 2xx → `entire_upstream_status`；非 JSON / schema 不符 / 空 body → `entire_upstream_malformed`。

## 构建

```bash
cd /mnt/新加卷/DSH/default-workspace/cpa-entire-plugin/quota

# 单元测试
go vet ./...
go test -count=1 ./...
go test -count=1 -race ./...

# 动态库（CPA 插件目录里文件名 = 插件 ID）
go build -buildmode=c-shared -o libentire_quota.so .

# 原生 ABI 冒烟（ctypes，mock host，合成 token，不触网）
python3 tools/abi_smoke.py ./libentire_quota.so
```

部署时把 `libentire_quota.so` 放进 CPA 的 `plugins` 目录（父目录受 host 的 `plugins.dir` 控制），
插件 ID 取 `libentire_quota` 去掉 `lib` 前缀后的 `entire_quota`；如希望 ID 直接是 `cpa-entire-quota`，
把产物重命名成 `cpa-entire-quota.so` 并在 `plugins.configs` 下用同名键即可。

## 管理端验证

```bash
# 列出已注册的 quota provider
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins | jq '.plugins[] | {id, quota_provider}'

# 读某个凭据的额度（auth_index 从 /v0/management/auth-files 取）
curl -s -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{"auth_index":"AUTH_INDEX"}' \
  http://127.0.0.1:8317/v0/management/plugins/cpa-entire-quota/quota | jq

# v8 前缀等价路由
curl -s -X POST -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{"auth_index":"AUTH_INDEX"}' \
  http://127.0.0.1:8317/v8/management/plugins/cpa-entire-quota/quota | jq
```

## 范围声明

本目录只实现 quota provider、认证解析、额度读取与测试。没有 Executor、没有推理反代、没有管理端 UI 路由、
没有任何写操作；`quota.reset` 固定返回 `success:false`，因为 Entire 公开契约里没有重置额度接口。
