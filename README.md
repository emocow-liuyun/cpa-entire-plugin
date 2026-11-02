# CPA × Entire

**状态：额度查询、OAuth 登录、模型目录，以及基于 Entire Agent 任务的文本推理中继均已接通并端到端验收（2026-11-02，`claude-haiku-5-5`）。**

推理中继是**任务式中继**：每次请求创建并归档一个 Agent，返回一条完整 `chat.completion`。
**没有原生 token 流、没有客户端工具调用、没有图片输入**；这些请求会被明确拒绝而不是伪装成功。详见 [EXECUTOR.md](EXECUTOR.md)。

## 当前组件

| 目录 | 能力 | 状态 |
| --- | --- | --- |
| `quota/` | CPA C ABI 插件；账户额度查询、Entire 设备授权接口、13 个模型目录，默认 `entire/` 前缀 | 已安装到测试者的 CPA；真实 Bearer 账单查询和模型列表验证通过；完整 CPA 登录卡片 → 浏览器批准 → 自动保存凭据 → 真实余额查询已验证；自动刷新轮换仍待验证 |
| `guard/` | 独立支出准入库；零余额冷却、串行预留、异常锁定、持久化 | 单元及 race 测试通过；尚未接入实际派单链路 |
| `quota/execute.go` | 推理中继执行器（`executor` 能力） | 已接入 CPA 并端到端验收；派单前读取账单，命中额度条件即拒绝 |
| `executor/` | 早期执行器研究骨架 | 历史研究代码，当前生产路径不使用 |
| `bridge/` | 实验性 Agent 控制面客户端 | 独立库；`StartRun` / `Run` 可创建计费任务，未接入 CPA |

## 已验证

- CLIProxyAPI v8 本机 dev 构建：C ABI v1、RPC schema 6；额度插件支持与宿主协商 schema。
- 隔离 CPA：5 个合成凭据，额度接口断言 12/12 通过，实际管理界面显示正余额、零余额、无效凭据错误。
- 各凭据独立使用自己的 token；自动充值目标不被当作消费上限。
- 官方设备授权流程已取得真实 Bearer，并导入独立 Entire 凭据；真实账单 GET 返回 HTTP 200，管理界面显示真实余额。
- `/v1/models` 返回 13 个 `entire/` 模型名称。
- `POST /v1/chat/completions`（`entire/claude-haiku-5-5`）返回真实模型答案，HTTP 200，`finish_reason: stop`，用量透传。
- `stream:true` 与 `tools` 均返回 HTTP 400 及明确原因，不产生上游派发。
- 所有认证文件、运行配置、访问令牌、刷新令牌和设备授权状态仅保存在本机，排除出仓库。

## 构建

额度插件需要 Go、CGO 和 C 编译器：

```bash
cd quota
go test -race ./...
go build -buildmode=c-shared -o entire_quota.so .
```

将动态库放进目标 CPA 的插件目录，在配置中单独添加（保留所有其他配置）：

```yaml
plugins:
  enabled: true
  configs:
    entire_quota:
      enabled: true
      priority: 5
      base-url: https://us.console.entire.io
      require-top-up-on-failure: true
```

额度能力选择 provider `entire`；模型使用凭据前缀 `entire`，例如 `entire/claude-haiku-5-5`。已有凭据的类型、前缀、禁用状态和所属主体需要核对，不要用其他提供商的凭据替代。

## 费用边界

Entire 任务按目标仓库所属的个人或组织计费，各主体余额独立。账单接口可能延迟数分钟，进行中的模型调用仍可完成。关闭自动充值并不等于消除超额月账单。

执行器在派单前读取账单，余额低于预留、余额未知、账单不可读、已有超额、自动充值开启或 runs 被暂停时直接拒绝，不创建 Agent。**这不是上游硬费用上限**：Entire 没有单次任务费用上限，用量延迟入账，在途调用仍可越过零余额。本地 `guard` 的预留同样只是风险收敛手段。

参考：[Entire 计费说明](https://docs.entire.com/billing.md)、[CLI 认证](https://docs.entire.com/cli-reference/auth.md)。

`bridge` 包含会启动计费任务的方法；它并非 Chat Completions 或真实 SSE 推理接口。不要仅因模型列表可见便开放生产推理流量。

## 执行器

`execution-enabled: true` 时插件注册 `executor`，把 `entire/<model>` 的 chat 请求中继为一次 Entire Agent
任务并返回标准 `chat.completion`。默认关闭；未开启或未配全目标时返回 `entire_execution_disabled`，不派发任何上游请求。
配置项、额度门禁与明确不支持的能力见 [EXECUTOR.md](EXECUTOR.md)。
