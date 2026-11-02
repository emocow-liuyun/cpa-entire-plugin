# 执行器适配：CPA → Entire 任务

本文件记录 2026-11-02 的真实验收。目标模型只用最低价 `claude-haiku-5-5`。

## 已验证

| 项 | 结果 |
| --- | --- |
| CPA `/v1/chat/completions` 正常返回 | HTTP 200，`application/json` |
| 实际答案 | `链路已通。`，`finish_reason: stop` |
| 模型 | `claude-haiku-5-5` |
| 用量透传 | prompt 59268 / completion 191 tokens |
| 临时 Agent 自动归档 | 每次请求后归档 |
| 单元测试 | `go test ./...`、`go vet ./...` 通过 |

## 不支持（明确拒绝，不伪装）

- **原生流式**：`execute_stream` 与 `stream:true` 返回 400 `entire_stream_unsupported`。每次任务只有一个完整结果。
- **客户端工具调用**：`tools`、`functions`、`tool_choice` 返回 400 `entire_tools_unsupported`。云端 Agent 在自己的沙箱里跑工具，不能回调客户端。
- **图片与音频**：`modalities`、`audio` 拒绝；消息内容只取文本部分。
- **`response_format` / `n>1`**：拒绝。
- **上下文长度**：会话渲染后超过 Agent 指令 16,384 字节上限即拒绝 `entire_context_too_large`。

## 额度保护

派单前必定读取一次账单，命中任一条件即拒绝，不创建 Agent、不消耗额度：

- 账单不可读或 `state != active`
- 余额未知
- `runs_paused.paused = true`
- 已有 overage > 0
- 自动充值开启（可能触发付费补款）
- 余额低于 `reserve-cents`（默认 100 分）

**这不是上游费用硬上限。** Entire 没有单次任务费用上限，用量延迟入账，在途调用可越过零余额。预留与提前拒绝只能缩小风险。

## 配置

```yaml
plugins:
  configs:
    entire_quota:
      execution-enabled: true
      cell-base-url: https://aws-us-east-2.api.entire.io
      project-host: gh
      project: emocow-liuyun
      repository-id: 01M6659ZFNBQ4B8AAMJJ7YVF8T
      trail-id: 01M66D75Q69HWD5HZ2CZEYYZ0T
      change-id: 01M66D75QCJYV651KPZSBGYBPC
      head-sha: <commit>
      reserve-cents: 100
      run-timeout-ms: 180000
```

`trail-id` / `change-id` 必须指向真实分支变更；同一 Trail 上并发请求会共享该目标，不同会话之间不隔离。

## 已知限制

- 每次请求创建并归档一个 Agent，延迟约 20 秒量级，不是低延迟推理通道。
- 会话被压平成单条指令文本；多轮上下文靠客户端每轮重发完整 message 数组。
- 输出若被 Entire 截断，`finish_reason` 为 `length`，但当前实现不会补全全文。
- prompt token 计数包含缓存创建与缓存读取，与模型原始计费口径可能不同。
