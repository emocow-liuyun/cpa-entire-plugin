# CPA × Entire

**状态：额度查询、OAuth 接口和模型目录已实现；通用推理代理尚未接通。** 不要将模型目录可见或单元测试通过理解为端到端推理已验收。

## 当前组件

| 目录 | 能力 | 状态 |
| --- | --- | --- |
| `quota/` | CPA C ABI 插件；账户额度查询、Entire 设备授权接口、13 个模型目录，默认 `entire/` 前缀 | 已安装到测试者的 CPA；真实 Bearer 账单查询和模型列表验证通过；OAuth RPC 有 mock 测试，完整 CPA 登录卡片流程仍待验证 |
| `guard/` | 独立支出准入库；零余额冷却、串行预留、异常锁定、持久化 | 单元及 race 测试通过；尚未接入实际派单链路 |
| `executor/` | 执行器研究骨架与请求校验 | 没有真实 Dispatcher，默认不派单；未安装到生产 CPA |
| `bridge/` | 实验性 Agent 控制面客户端 | 独立库；`StartRun` / `Run` 可创建计费任务，未接入 CPA |

## 已验证

- CLIProxyAPI v8 本机 dev 构建：C ABI v1、RPC schema 6；额度插件支持与宿主协商 schema。
- 隔离 CPA：5 个合成凭据，额度接口断言 12/12 通过，实际管理界面显示正余额、零余额、无效凭据错误。
- 各凭据独立使用自己的 token；自动充值目标不被当作消费上限。
- 官方设备授权流程已取得真实 Bearer，并导入独立 Entire 凭据；真实账单 GET 返回 HTTP 200，管理界面显示真实余额。
- `/v1/models` 已返回 13 个 `entire/` 模型名称。模型元数据注明推理适配器未接通。
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

**当前未完成零额度自动冷却的端到端派单控制，也不保证零额外账单。** 本地 `guard` 的预留不是上游硬费用上限。尚缺计费主体匹配、真实执行器连接、取消和不确定提交对账等验收。发布源码不表示这些功能已经完成。

参考：[Entire 计费说明](https://docs.entire.com/billing.md)、[CLI 认证](https://docs.entire.com/cli-reference/auth.md)。

`bridge` 包含会启动计费任务的方法；它并非 Chat Completions 或真实 SSE 推理接口。不要仅因模型列表可见便开放生产推理流量。
