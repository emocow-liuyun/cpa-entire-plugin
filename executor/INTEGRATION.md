# Entire Executor：真实执行的整合方案（设计，未启用）

本文描述**如果**要把 `505` 变成真实响应，需要接上什么、按什么顺序接、以及每一项如何验收。
当前仓库里的 `libentire_executor.so` **没有**安装任何 dispatcher：`executor.execute` 在所有闸门
都打开时仍返回 `entire_real_execution_not_implemented` / HTTP 503。

本文不声称任何一步已经完成。每一步都标了「现状」，未做未验证的一律写「未验证」。

---

## 0. 当前事实（来自本工作区既有验证）

| 事项 | 状态 | 来源 |
| --- | --- | --- |
| CPA 插件 C ABI v1 加载/调用 | 已验证（本机 ctypes + mock host） | `tools/abi_smoke.py` |
| `executor.execute` 的 RPC 字段名与 base64 编码 | 已按 `sdk/pluginapi` 对齐 | `../cpa-entire-feasibility/cpa-source/sdk/pluginapi/types.go` |
| Entire 有 Agent 配置 / 启动 run / 查询 run 的 HTTP 接口 | 已通过在线 OpenAPI 确认路径 | `cpa-entire-feasibility/evidence/{core,cell}-openapi.json` |
| Entire 有 OpenAI 兼容的 Chat Completions 推理路由 | **未发现**。公开规范里没有 | 同上 |
| run 的输出文本 | `RunOutput.text` + `truncated` 字段 | `evidence/selected-schemas.json` |
| run 是否可绑定“每次调用”的上下文 | `RunTargetInput` 只收 `trail_id` / `change_id` / `head_sha` | 同上 |
| 单次 run 的费用上限 | **不存在**硬上限；按 usage 结算 | `evidence/model-connectivity-results.json` 只记录结算后余额 |
| 计费结算延迟 | 观察到完成时余额未变，usage 可能之后才结算 | 同上 |

结论：Entire 侧能拿到的是「一次云端 Agent run 的最终文本」，不是「一次模型推理的 token 流」。
下面的方案就是按这个事实设计的，不做能力夸大。

---

## 1. 接口：`Dispatcher`

`plugin.go` 定义了唯一接缝：

```go
type Dispatcher interface {
	Dispatch(ctx context.Context, call DispatcherContext, prompt string) (DispatcherResult, error)
}

type DispatcherContext struct {
	Model      string
	Harness    string        // trail-review
	AuthID     string
	HostClient HostHTTPClient // 通过 host.http.do 走宿主传输策略
	Config     runtimeConfig  // 已通过全部闸门
	Credential credential     // 已解析、已登记脱敏
}

type DispatcherResult struct {
	Text      string
	Truncated bool
	RunID     string
	Usage     map[string]int64 // prompt_tokens / completion_tokens / total_tokens
}
```

约束（由 `execute.go` 保证，实现方不需要重复处理）：

- 只有在第 1–10 条闸门全部通过后才会被调用；
- `ctx` 已带 30 分钟超时；
- 返回的 `Text` 会被渲染成标准 chat-completions 响应；
- `Truncated: true` 会被渲染成 `finish_reason: "length"`，不会被伪装成 `stop`；
- 返回的 error 若不是 `*pluginError`，会自动落成 HTTP 502；
- 任何字符串在离开插件前都会过脱敏表，实现方不需要自己过滤 token。

### 安装位置

```go
// 仅在明确启用真实执行时调用；默认不调用。
func installDispatcher(d Dispatcher) { setDispatcher(d) }
```

建议放在一个**独立构建标签**后面，例如 `//go:build entire_realexec`，这样默认产出的
`libentire_executor.so` 在二进制层面就不包含真实派单代码。

---

## 2. 接入 `../bridge` 的路线（推荐）

`../bridge` 已经实现了需要的控制面调用，并且已经做了同源校验、凭据不落日志、错误体脱敏。
把它包进 `Dispatcher` 即可，不要复制它的代码，也不要改它（它由其他代理维护）。

### 2.1 需要 bridge 暴露的调用

按 `../bridge` 现有 doc.go 的接口面：

| 步骤 | bridge 方法 | 说明 |
| --- | --- | --- |
| 读账单 | `GetBilling(ctx, refresh)` | 派单前读一次，确认 `runs_paused` 与余额 |
| 启动 run | `StartRun(ctx, ...)` | `POST /{host}/{project}/agents/{id}/runs`，带 `Idempotency-Key` |
| 轮询 | `PollRun(ctx, runID)` | 轮询到终态，读 `output.text` / `output.truncated` / `usage_by_model` |
| 取消 | `CancelRun(ctx, runID)` | ctx 超时时尽力取消，避免继续计费 |

如果某个方法名与实际不符，以 `../bridge` 的实际导出为准；本文只约定语义。

### 2.2 适配器骨架

```go
//go:build entire_realexec

package main

import (
	"context"
	"errors"
	"strings"

	"github.com/yg/cpa-entire-plugin/bridge"
)

type bridgeDispatcher struct{}

func (bridgeDispatcher) Dispatch(ctx context.Context, call DispatcherContext, prompt string) (DispatcherResult, error) {
	cfg := call.Config

	client, errClient := bridge.New(bridge.Config{
		CoreBaseURL: cfg.CoreBaseURL,
		CellBaseURL: cfg.CellBaseURL,
		Bearer:      call.Credential.Token,
		ProjectHost: cfg.ProjectHost,
		Project:     cfg.Project,
		AgentID:     cfg.AgentID,
		TrailID:     cfg.TrailID,
		ChangeID:    cfg.ChangeID,
	})
	if errClient != nil {
		return DispatcherResult{}, newPluginError(codeExecutionDisabled,
			"bridge client rejected the configuration: "+errClient.Error(), 503)
	}

	// 1) 派单前账单复查：paused 或余额不足则拒绝，绝不下单。
	billing, errBilling := client.GetBilling(ctx, true)
	if errBilling != nil {
		return DispatcherResult{}, newPluginError(codeBillingUnavailable,
			"billing re-check failed before dispatch: "+errBilling.Error(), 503)
	}
	if billing.RunsPaused != nil && billing.RunsPaused.Paused {
		return DispatcherResult{}, newPluginError(codeBillingUnavailable,
			"Entire reports runs paused ("+billing.RunsPaused.Reason+"); nothing was dispatched", 503)
	}
	if errBudget := checkLocalBook(cfg, billing); errBudget != nil {
		return DispatcherResult{}, errBudget
	}

	// 2) 启动 run。prompt 来自 chat-completions 的纯文本。
	runID, errStart := client.StartRun(ctx, bridge.RunRequest{
		Instructions: prompt, // 见 §3 的长度约束
	})
	if errStart != nil {
		return DispatcherResult{}, startError(errStart)
	}

	// 3) 轮询到终态。
	run, errPoll := client.PollRun(ctx, runID)
	if errPoll != nil {
		// 超时/取消时尽力取消，避免 run 继续计费。
		_ = client.CancelRun(context.WithoutCancel(ctx), runID)
		return DispatcherResult{}, newPluginError(codeNotImplemented,
			"run did not reach a terminal state: "+errPoll.Error(), 504)
	}
	switch run.Status {
	case "succeeded":
	case "canceled", "failed", "timed_out":
		return DispatcherResult{}, newPluginError("entire_run_failed",
			"run ended "+run.Status+" ("+run.RunID+")", 502)
	default:
		return DispatcherResult{}, newPluginError("entire_run_failed",
			"run ended in unexpected status "+run.Status, 502)
	}

	text := strings.TrimSpace(run.Output.Text)
	if text == "" {
		return DispatcherResult{}, newPluginError("entire_run_empty",
			"run succeeded but produced no output text", 502)
	}

	return DispatcherResult{
		Text:      text,
		Truncated: run.Output.Truncated,
		RunID:     run.RunID,
		Usage: map[string]int64{
			"prompt_tokens":     run.Usage.InputTokens,
			"completion_tokens": run.Usage.OutputTokens,
		},
	}, nil
}

var errMissingBridge = errors.New("bridge adapter is not wired")
```

`checkLocalBook` 与 `startError` 属于实现方，需要：

- `checkLocalBook`：在本地记一笔「派单前余额快照 + 预留额度」，语义与 `../guard` 的内核一致
  （单账户串行、余额未知即拒绝、自动充值开启即拒绝、overage>0 即拒绝）。**直接复用 `../guard`
  的语义，不要复制它的文件**——把它作为独立模块 import 进来，或者在 adapter 包内实现同等判定并
  用相同测试向量验证。
- `startError`：把 bridge 的 `ErrOriginNotAllowed` / `ErrInsecureEndpoint` / `ErrMissingBearer`
  映射成 503，把上游 4xx 映射成 502 并保留脱敏后的原因。

---

## 3. 必须解决的实际约束（这些决定了方案能否成立）

### 3.1 上下文注入是配置级，不是请求级

`RunTargetInput` 只接受 `trail_id` / `change_id` / `head_sha`，没有 `messages` / `input` 字段。
要让模型看到用户这一轮的 prompt，只有两条路：

1. **写进 Agent 配置**（`PUT .../agents/{id}`，带 `If-Match`）。这会把每次请求变成一次全局配置
   改写：并发请求必然互相覆盖，而且 `AgentCreate.instructions` 上限 16384 字符。
2. **把 prompt 编码进 target**（例如写进 `head_sha` 之外的位置）——规范里没有这样的字段。

**现状：两条路都没有验证过。** 能否成立取决于 Entire 是否允许把用户文本放进其他可写字段。
没有实测证据之前，任何「直接当模型反代用」的说法都是猜测。

建议的第一步实测（只做一次，用小任务，观测账单）：

- 用一个测试项目，把一句固定文本写进 Agent 的 `instructions`；
- 启动一次 run，读回 `output.text`；
- 确认文本确实进入了上下文、`truncated` 为 false、usage 与预期同数量级。

### 3.2 没有 token 流

run 结束才有完整文本。所以本 executor 只声明 `chat-completions` 的非流式输出，并且
`executor.execute_stream` 直接拒绝。任何把最终文本切块称为「SSE 流式推理」的实现都是误导。

### 3.3 客户端工具调用没有回传通道

云端 Agent 在自己的沙箱里跑工具（`sandbox.repo_token: read` 等）。它无法把 `tool_calls` 交回客户端、
再等客户端返回结果。因此 `request.go` 里的工具拒绝是**结构性**的，不是策略选择：
接入真实执行之后，这个拒绝仍然必须保留。

### 3.4 计费没有硬上限

Entire 按用量结算，控制面不提供单次费用上限。这意味着：

- `billing-limit-enforced` 只能表达运维的预算承诺；
- 派单前读到的余额是**快照**，run 期间仍可能超额；
- 观察到结算有滞后（完成时余额未变），所以「跑完看余额」不能作为事前准入。

因此真实执行必须建立在三件事上：

1. **保守预留**：按 usage 的历史高分位预留，结算后回填（`../guard` 的 high-water-mark 语义）；
2. **单账户串行**：同一账户同时只允许一个 in-flight run（否则预留不可计算）；
3. **poison-on-failure**：预留无法持久化时，后续一律拒绝，直到人工核对。

`allow-unbounded-spend` 的存在就是为了让「接受这一风险」成为一个必须写下来的显式动作，
而不是默认行为。

### 3.5 排队与首字延迟

run 有 `queued` / `running` 状态，云端沙箱要启动、拉仓库、读文件。延迟与普通模型 API 不在
一个量级。`dispatchTimeout` 现在是 30 分钟；接入后应改为按 `timeout_ms`（runner 配置里是
1500000 ≈ 25 分钟）再加余量，并在超时时**取消** run 而不是让它继续跑。

---

## 4. 上线顺序与验收

每一步都必须带可观测证据，前一步没通过就不进入下一步。

| 阶段 | 动作 | 验收 | 风险 |
| --- | --- | --- | --- |
| P0（当前） | 冻结：只发 fail-closed 版本 | `build.sh` 全绿；全闸门打开仍 503 | 无 |
| P1 | 只读联调：`auth.parse` + 一次 `GET /me/billing` | 账单字段读数正确；token 不出现在任何日志 | 无（只读） |
| P2 | 单次最小 run：固定 prompt，人工触发 | run `succeeded`，`output.text` 非空，`truncated=false` | 产生一次真实费用；需事先确认余额 |
| P3 | 计费标定：同一 prompt 重复 N 次 | usage 方差、结算延迟窗口、余额变化量能算出来 | N 次费用 |
| P4 | 准入内核：接 `../guard` 语义，单账户串行 + 预留 | 并发压测下预留不重叠；崩溃后不重复派单 | 逻辑错误会导致超额 |
| P5 | 灰度：只对白名单项目开门 | 只有白名单模型/项目可派单；其余 503 | 误开门 |
| P6 | 观测：usage 上报、`finish_reason` 与 `truncated` 一致 | `finish_reason=length` 恰好在 `truncated=true` 时出现 | 静默截断被当完整答案 |

P2 之前不要打开 `allow-unbounded-spend`。

---

## 5. 必须保留的拒绝（接入真实执行后也不放宽）

- 工具、图片、音频、文件、未知 content type；
- `stream: true` 与 `executor.execute_stream`；
- `response_format` / `n>1` / `logprobs` / `reasoning_effort`；
- 模型不在 `entire-review-*` 命名空间；
- 任一闸门未开。

理由不是保守，而是这些请求**没有可用的执行语义**：上游没有对应能力，接受它们只能靠伪造结果。

---

## 6. 需要实测才能定论的问题

以下问题在本工作区内没有答案。任何实现都必须先测出来，不能假设：

1. 用户 prompt 能否进入 run 的上下文，通过哪个字段，长度上限多少？
2. `output.text` 的 `truncated` 阈值是什么？超长输出能否分页/续读？
3. `usage_by_model` 的字段是否能稳定映射成 OpenAI `usage`？多模型 run 怎么合并？
4. run 期间余额被其他活动消耗时，Entire 会拦截还是继续跑到结束？
5. `Idempotency-Key` 的保留窗口多长？重试会不会产生第二次计费？
6. 取消的语义：cancel 之后已产生的 token 是否照常计费？
7. Agent 配置改写（`PUT`）与正在运行的 run 是否冲突？
