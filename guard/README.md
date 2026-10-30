# CPA Entire — 支出准入内核（`guard`）

独立 Go 库：CPA Entire 插件的广告账户支出准入与预留内核。仅使用 Go 标准库，无外部依赖。不接入 CPA 平台或 GitHub；持久化到单个 JSON 状态文件。

> **声明**：本内核**不宣传保证无超支**。外部活动（广告平台侧扣费、延迟到账的扣费上报、自动充值触发）与网络延迟都会破坏任何此类保证。内核只做本地保守准入决策：高水位预留、fail-closed、崩溃后锁定待人工对账。

## 设计要点

| 需求 | 实现 |
|---|---|
| 单账户序列化 | 每账户最多一个活跃 `Reservation`；有活跃预留时 `Reserve` 返回 `ErrReservationActive` |
| 单实例限制 | `NewKernel`/`LoadKernel` 对状态文件加 `flock` 独占锁；第二个实例返回 `ErrInstanceActive`，`Close` 释放 |
| 余额为零/低于保留金冷却 | 进入 5 分钟冷却（`cooldownUntil`），期间返回 `ErrCooldown`；同时投影 overage |
| 余额未知/不可用 fail-closed | `Available == nil` → `ErrBalanceUnknown`，一律拒绝 |
| 自动充值开启一律阻断 | `AutoRecharge = true` → 无条件 `ErrAutoRechargeEnabled`；`nil`（未知）→ `ErrAutoRechargeUnknown`；仅显式 `false` 才准入 |
| overage 未知或 > 0 阻断 | `Overage == nil` 或 `Overage > 0` → `ErrOveragePositive`；只有明确观测为 0 才继续准入检查。自动充值检查优先执行 |
| RunsPaused fail-closed | `RunsPaused = true` 或 `nil`（未知）→ `ErrRunsPaused` |
| 未知安全字段阻断/拒绝 | 状态文件含未知字段（顶层或账户级）→ `ErrCorruptState` 拒绝加载；三元安全字段（AutoRecharge/RunsPaused）未知值一律阻断 |
| ReservePerDispatch 必须 > 0 | `SetAccount` 与状态加载均校验，`<= 0` 返回 `ErrBadInput` / `ErrCorruptState` |
| 拒绝负数 charge | `Settle(charged < 0)`、负 `Available`、负 `Overage`、负对账余额均返回 `ErrBadInput` |
| 状态保存失败阻止后续运行 | 落盘失败即毒化内核（`ErrPersistFailed`），拒绝后续准入与变更；保存可能已 rename，内存保留保守状态，重启前需核对磁盘与实际派单 |
| 设置账户不覆盖未结算预算 | 存在活跃（未结算）预留时，`SetAccount` 保留原 `Available` 与 `ReservePerDispatch` |
| 保守高水位预留 | 预留成功即把余额减去保留金；`Settle` 时按实际扣费退差额 |
| 持久状态原子保存 | 同目录临时文件写入并 Sync，再 rename 和目录 Sync；任何失败毒化。文件系统和硬件决定最终持久性，残留临时文件阻断加载待核对 |
| 崩溃/请求不确定仍锁定 | 预留持久化后重启仍存在，阻止重复派单；`MarkDispatched` 后对不确定结果 `Settle` 会锁定账户（`Locked`），只能 `Reconcile` 显式人工对账解锁 |

## API（`package guard`）

```go
type Amount int64 // 次级货币单位（如分），全部整数运算

func NewKernel(path string) (*Kernel, error)   // 持锁后加载已有状态；缺失时为空状态
func LoadKernel(path string) (*Kernel, error)  // 从磁盘恢复；文件缺失返回空内核；拒绝未知字段
func (k *Kernel) Close() error                 // 释放单实例文件锁

func (k *Kernel) SetAccount(id string, a Account) error
// Account{Available *Amount, AutoRecharge *bool, Overage *Amount,
//         ReservePerDispatch Amount, RunsPaused *bool}
// ReservePerDispatch 必须 > 0。存在未结算预留时保留原预算快照。

func (k *Kernel) SetOverage(accountID string, overage Amount) error
// 记录最新投影超支；nil（未知）或 > 0 均阻断，核实为 0 后调用 SetOverage(id, 0)。

func (k *Kernel) Reserve(accountID string) (string, error)
// 准入一笔派单。按序检查：PersistFailed → Locked → Active → Cooldown
// → AutoRecharge(nil/true 拒绝) → Overage(nil 或 >0 拒绝) → RunsPaused(nil/true 拒绝)
// → Balance unknown → Below reserve(设冷却+投影 overage)。成功返回预留 ID。

func (k *Kernel) MarkDispatched(accountID, reservationID string) error
// 标记派单已实际开始；此后崩溃属于"结果不确定"。

func (k *Kernel) Settle(accountID, reservationID string, charged *Amount) error
// 以实际扣费结算并清除预留，退回（预留 - 实扣）差额；负数 charged 拒绝。
// charged == nil 且已 MarkDispatched → 账户锁定，返回 ErrNotSettled。
// 未派发（phase unknown）且 charged == nil → 全额退回预留，清除 overage。

func (k *Kernel) Reconcile(accountID string, newBalance Amount) error
// 显式人工对账：解锁账户、以核实后的余额覆盖、清除活跃预留、overage 与冷却。

func (k *Kernel) Status(accountID string) (Account, bool, error) // 深拷贝
func (k *Kernel) LockedAccounts() ([]string, error) // 待对账账户，按 id 排序；关闭后 ErrClosed
```

### 错误

`ErrAccountLocked` / `ErrReservationActive` / `ErrBalanceUnknown` / `ErrBalanceBelowReserve` / `ErrAutoRechargeEnabled` / `ErrAutoRechargeUnknown` / `ErrOveragePositive` / `ErrRunsPaused` / `ErrCooldown` / `ErrNoReservation` / `ErrNotSettled` / `ErrPersistFailed` / `ErrInstanceActive` / `ErrCorruptState` / `ErrBadInput`（`ErrAutoRechargeOverage` 为兼容别名，等价 `ErrOveragePositive`）。所有错误用 `errors.Is` 判断。

### 准入决策顺序（fail-closed 优先级）

1. 内核已因落盘失败毒化 → 拒绝（`ErrPersistFailed`）
2. 账户锁定 → 拒绝（需 `Reconcile`）
3. 已有活跃预留 → 拒绝（序列化）
4. 冷却中 → 拒绝
5. `AutoRecharge` 为 `true` 或 `nil` → 拒绝
6. `Overage == nil`（未知）或 `Overage > 0` → 拒绝（均返回 `ErrOveragePositive`）
7. `RunsPaused` 为 `true` 或 `nil` → 拒绝
8. 余额未知 → 拒绝
9. 余额 < 保留金（含 0）→ 设冷却、投影 overage 并拒绝
10. 否则预留：余额减去保留金（高水位），预留持久化后返回 ID

`Settle` 成功或 `Reconcile` 会将 `Overage` 置为 `nil`；这表示未知而非已验证为零。重新派单前须核实最新 overage 并通过 `SetOverage(accountID, 0)` 显式记录零值。

## 崩溃与不确定请求语义

- `Reserve` 成功后状态**先持久化**再返回，进程崩溃重启后预留仍在，阻止对同一账户重复派单。
- `MarkDispatched` 之后若扣费结果未知（进程死亡、响应丢失），`Settle(id, nil)` 将账户置 `Locked` 并返回 `ErrNotSettled`——锁定态下所有准入被拒，直到操作员通过 `Reconcile` 写入核实余额。
- `Settle` 在派发未开始时（phase = unknown）以 `nil` 结算视为全额退回，无需对账。
- 落盘失败后内核毒化：无间隔地阻断一切后续运行，防止在状态未记录的情况下继续派单。

## 运行

```bash
cd guard
go test ./...        # 确定性测试（注入时钟与 ID 生成器）
go test -race ./...  # 并发安全
go vet ./...
```

## 测试覆盖（`guard_test.go`）

预留/结算闭环与持久化往返、单账户序列化（含并发准入恰一次）、零余额与低于保留金冷却、未知余额 fail-closed、自动充值开启/未知值一律阻断、overage 未知及 > 0 阻断、零值放行（含结算/对账后重新验证、`SetOverage` 与低于保留金自动投影）、RunsPaused 显式/未知阻断、`ReservePerDispatch <= 0` 拒绝、负数 charge/余额/对账值拒绝、状态文件未知字段拒绝、崩溃后预留仍锁定 + 显式对账解锁、不确定请求保持锁定、未派发全额退回、落盘失败毒化内核并阻断后续运行（重启后新实例可恢复）、单实例文件锁、活跃预留期间 `SetAccount` 保留预算快照、原子保存无残留临时文件、损坏状态文件拒绝、冷却到期恢复。
