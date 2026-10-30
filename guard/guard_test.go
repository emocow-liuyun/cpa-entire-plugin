package guard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func A(i int64) *Amount { v := Amount(i); return &v }

func B(b bool) *bool { return &b }

func newTestKernel(t *testing.T) (*Kernel, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	k, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	// Deterministic clock and ids.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := 0
	k.nowFn = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }
	n := 0
	k.idFn = func() string { n++; return "rsv-test-" + string(rune('a'+n-1)) }
	return k, path
}

// cfg returns a default valid config with the auto-recharge and pause flags
// explicitly known-false and overage explicitly zero.
func cfg(bal int64, reserve int64) Account {
	return Account{
		Available:          A(bal),
		AutoRecharge:       B(false),
		Overage:            A(0),
		ReservePerDispatch: Amount(reserve),
		RunsPaused:         B(false),
	}
}

func TestReserveAndSettleHappyPath(t *testing.T) {
	k, path := newTestKernel(t)
	if err := k.SetAccount("acc1", cfg(10000, 1000)); err != nil {
		t.Fatal(err)
	}
	id, err := k.Reserve("acc1")
	if err != nil {
		t.Fatal(err)
	}
	if id != "rsv-test-a" {
		t.Fatalf("unexpected id %q", id)
	}
	a, _, _ := k.Status("acc1")
	if a.Available == nil || *a.Available != 9000 {
		t.Fatalf("high-water-mark not applied: %v", a.Available)
	}
	if err := k.Settle("acc1", id, A(700)); err != nil {
		t.Fatal(err)
	}
	a, _, _ = k.Status("acc1")
	if a.Active != nil {
		t.Fatal("reservation should be cleared after settle")
	}
	if *a.Available != 9300 { // 9000 + (1000-700)
		t.Fatalf("balance after settle = %d, want 9300", *a.Available)
	}
	// Reload from disk to confirm persistence.
	k.Close()
	k2, err := LoadKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Close()
	a2, _, _ := k2.Status("acc1")
	if a2.Active != nil || *a2.Available != 9300 {
		t.Fatalf("state not persisted correctly: %+v", a2)
	}
}

func TestSingleAccountSerialization(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(5000, 1000))
	if _, err := k.Reserve("acc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrReservationActive) {
		t.Fatalf("want ErrReservationActive, got %v", err)
	}
}

func TestCooldownOnZeroAndBelowReserve(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc0", cfg(0, 1000))
	_, err := k.Reserve("acc0")
	if !errors.Is(err, ErrBalanceBelowReserve) {
		t.Fatalf("zero balance: want ErrBalanceBelowReserve, got %v", err)
	}
	a, _, _ := k.Status("acc0")
	if a.CooldownUntil.IsZero() {
		t.Fatal("cooldown not set on zero balance")
	}
	if _, err := k.Reserve("acc0"); !errors.Is(err, ErrCooldown) {
		t.Fatalf("during cooldown: want ErrCooldown, got %v", err)
	}

	_ = k.SetAccount("accLow", cfg(500, 1000))
	_, err = k.Reserve("accLow")
	if !errors.Is(err, ErrBalanceBelowReserve) {
		t.Fatalf("below reserve: want ErrBalanceBelowReserve, got %v", err)
	}
	if _, err := k.Reserve("accLow"); !errors.Is(err, ErrCooldown) {
		t.Fatalf("below reserve during cooldown: want ErrCooldown, got %v", err)
	}
}

func TestFailClosedUnknownBalance(t *testing.T) {
	k, _ := newTestKernel(t)
	c := cfg(0, 1000)
	c.Available = nil
	_ = k.SetAccount("acc1", c)
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrBalanceUnknown) {
		t.Fatalf("want ErrBalanceUnknown, got %v", err)
	}
}

func TestAutoRechargeEnabledBlocksUnconditionally(t *testing.T) {
	// Auto-recharge enabled: block regardless of balance, even above reserve.
	k, _ := newTestKernel(t)
	c := cfg(20000, 1000)
	c.AutoRecharge = B(true)
	_ = k.SetAccount("acc1", c)
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrAutoRechargeEnabled) {
		t.Fatalf("auto-recharge ON must always block, got %v", err)
	}

	// Auto-recharge unknown (nil): fail-closed block.
	k2, _ := newTestKernel(t)
	c2 := cfg(20000, 1000)
	c2.AutoRecharge = nil
	_ = k2.SetAccount("acc2", c2)
	if _, err := k2.Reserve("acc2"); !errors.Is(err, ErrAutoRechargeUnknown) {
		t.Fatalf("auto-recharge unknown must block, got %v", err)
	}

	// Auto-recharge disabled with sufficient balance admits normally.
	k3, _ := newTestKernel(t)
	_ = k3.SetAccount("acc3", cfg(2000, 1000))
	if _, err := k3.Reserve("acc3"); err != nil {
		t.Fatalf("auto-recharge OFF with sufficient balance should admit, got %v", err)
	}
}

func TestOveragePositiveBlocksUnconditionally(t *testing.T) {
	k, _ := newTestKernel(t)
	c := cfg(20000, 1000)
	c.Overage = A(1)
	_ = k.SetAccount("acc1", c)
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrOveragePositive) {
		t.Fatalf("overage > 0 must block unconditionally, got %v", err)
	}

	// An unknown overage blocks without reserving funds or mutating state.
	k2, _ := newTestKernel(t)
	c2 := cfg(2000, 1000)
	c2.Overage = nil
	if err := k2.SetAccount("acc2", c2); err != nil {
		t.Fatal(err)
	}
	if id, err := k2.Reserve("acc2"); !errors.Is(err, ErrOveragePositive) || id != "" {
		t.Fatalf("unknown overage must block with ErrOveragePositive and no ID, got %q, %v", id, err)
	}
	before, ok, _ := k2.Status("acc2")
	if !ok || before.Active != nil || before.Available == nil || *before.Available != 2000 || before.Overage != nil {
		t.Fatalf("blocked reservation changed account: %+v", before)
	}
	if err := k2.SetOverage("acc2", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Reserve("acc2"); err != nil {
		t.Fatalf("verified zero overage should allow admission, got %v", err)
	}
	k3, _ := newTestKernel(t)
	c3 := cfg(2000, 1000)
	c3.Overage = A(0)
	_ = k3.SetAccount("acc3", c3)
	if _, err := k3.Reserve("acc3"); err != nil {
		t.Fatalf("zero overage must not block, got %v", err)
	}
}

func TestSettlementClearsOverageToUnknownAndBlocks(t *testing.T) {
	k, _ := newTestKernel(t)
	if err := k.SetAccount("acc", cfg(2000, 1000)); err != nil {
		t.Fatal(err)
	}
	id, err := k.Reserve("acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Settle("acc", id, A(500)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Reserve("acc"); !errors.Is(err, ErrOveragePositive) {
		t.Fatalf("settlement leaves overage unknown and must block, got %v", err)
	}
	if err := k.SetOverage("acc", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Reserve("acc"); err != nil {
		t.Fatalf("verified zero overage should allow admission, got %v", err)
	}
}

func TestSetOverageBlocksAndSettleClears(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(2000, 1000))
	if err := k.SetOverage("acc1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrOveragePositive) {
		t.Fatalf("want ErrOveragePositive after SetOverage, got %v", err)
	}
	// A below-reserve read projects an overage too.
	k2, _ := newTestKernel(t)
	_ = k2.SetAccount("acc2", cfg(100, 1000))
	_, _ = k2.Reserve("acc2")
	a, _, _ := k2.Status("acc2")
	if a.Overage == nil || *a.Overage != 900 {
		t.Fatalf("below-reserve must project overage, got %v", a.Overage)
	}
}

func TestRunsPausedBlocks(t *testing.T) {
	// Explicitly paused.
	k, _ := newTestKernel(t)
	c := cfg(20000, 1000)
	c.RunsPaused = B(true)
	_ = k.SetAccount("acc1", c)
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrRunsPaused) {
		t.Fatalf("want ErrRunsPaused when paused, got %v", err)
	}

	// Pause state unknown: fail-closed.
	k2, _ := newTestKernel(t)
	c2 := cfg(20000, 1000)
	c2.RunsPaused = nil
	_ = k2.SetAccount("acc2", c2)
	if _, err := k2.Reserve("acc2"); !errors.Is(err, ErrRunsPaused) {
		t.Fatalf("want ErrRunsPaused when pause unknown, got %v", err)
	}
}

func TestReservePerDispatchMustBePositive(t *testing.T) {
	k, _ := newTestKernel(t)
	if err := k.SetAccount("acc1", Account{Available: A(100), ReservePerDispatch: 0}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("ReservePerDispatch=0 must be rejected, got %v", err)
	}
	if err := k.SetAccount("acc1", Account{Available: A(100), ReservePerDispatch: -5}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("negative ReservePerDispatch must be rejected, got %v", err)
	}
}

func TestNegativeChargeRejected(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(10000, 1000))
	id, err := k.Reserve("acc1")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Settle("acc1", id, A(-1)); !errors.Is(err, ErrBadInput) {
		t.Fatalf("negative charge must be rejected, got %v", err)
	}
	// Reservation must still be settleable with a valid charge.
	if err := k.Settle("acc1", id, A(500)); err != nil {
		t.Fatalf("valid settle after rejected negative: %v", err)
	}
}

func TestNegativeBalancesRejected(t *testing.T) {
	k, _ := newTestKernel(t)
	c := cfg(100, 100)
	c.Available = A(-10)
	if err := k.SetAccount("acc1", c); !errors.Is(err, ErrBadInput) {
		t.Fatalf("negative Available must be rejected, got %v", err)
	}
	if err := k.Reconcile("x", -1); !errors.Is(err, ErrBadInput) {
		t.Fatalf("negative reconcile balance must be rejected, got %v", err)
	}
}

func TestUnknownFieldsInStateFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Write a state containing an unknown top-level and account field.
	raw := `{"accounts":{"acc1":{"available":100,"reservePerDispatch":10,"futureField":1}},"unknownTop":true}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadKernel(path)
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("unknown safety fields must be rejected, got %v", err)
	}
	// A clean state still loads.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	k := NewKernelNoLock(path)
	defer k.Close()
	_ = k.SetAccount("a", cfg(10, 5))
	k.Close()
	k2, err := LoadKernel(path)
	if err != nil {
		t.Fatalf("clean state must load, got %v", err)
	}
	k2.Close()
}

func TestPersistFailureBlocksFurtherRuns(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(10000, 1000))
	// Break persistence.
	k.failSaveFn = func() error { return errors.New("disk gone") }
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("want ErrPersistFailed on failed reserve save, got %v", err)
	}
	// Kernel is poisoned: even a repaired save path must not resume runs.
	k.failSaveFn = nil
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("kernel must stay poisoned, got %v", err)
	}
	if err := k.SetAccount("acc1", cfg(10000, 1000)); !errors.Is(err, ErrPersistFailed) {
		t.Fatalf("mutations must also be denied after poison, got %v", err)
	}
	a, _, _ := k.Status("acc1")
	if a.Active == nil || *a.Available != 9000 {
		t.Fatal("failed reserve must conservatively retain the in-memory hold")
	}
	// A second kernel on the same state file is a fresh instance and may run.
	k.Close()
	k2, err := LoadKernel(k.path)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Close()
	if _, err := k2.Reserve("acc1"); err != nil {
		t.Fatalf("fresh instance after restart must admit, got %v", err)
	}
}

func TestSingleInstanceFileLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	k1, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k1.Close()
	if _, err := NewKernel(path); !errors.Is(err, ErrInstanceActive) {
		t.Fatalf("second kernel instance must be rejected, got %v", err)
	}
	if _, err := LoadKernel(path); !errors.Is(err, ErrInstanceActive) {
		t.Fatalf("second LoadKernel must be rejected, got %v", err)
	}
	// After Close, the file is available again.
	k1.Close()
	k2, err := NewKernel(path)
	if err != nil {
		t.Fatalf("after Close a new kernel must acquire the lock: %v", err)
	}
	k2.Close()
}

func TestSetAccountPreservesUnsettledBudget(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(10000, 1000))
	id, err := k.Reserve("acc1")
	if err != nil {
		t.Fatal(err)
	}
	// Reconfigure with a wildly different balance/reserve while a
	// reservation is un-settled: the settled budget snapshot must survive.
	if err := k.SetAccount("acc1", cfg(999999, 777)); err != nil {
		t.Fatal(err)
	}
	a, _, _ := k.Status("acc1")
	if a.Active == nil || a.Active.ID != id {
		t.Fatalf("reservation lost across reconfig: %+v", a)
	}
	if *a.Available != 9000 {
		t.Fatalf("unsettled budget overwritten: available = %d, want 9000", *a.Available)
	}
	if a.ReservePerDispatch != 1000 {
		t.Fatalf("reserve changed under active reservation: %d, want 1000", a.ReservePerDispatch)
	}
	// After settle, reconfiguration applies normally.
	if err := k.Settle("acc1", id, A(1000)); err != nil {
		t.Fatal(err)
	}
	if err := k.SetAccount("acc1", cfg(500, 250)); err != nil {
		t.Fatal(err)
	}
	a, _, _ = k.Status("acc1")
	if a.ReservePerDispatch != 250 || *a.Available != 500 {
		t.Fatalf("reconfig after settle not applied: %+v", a)
	}
}

func TestCrashKeepsReservationLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// First process: reserve then "crash" without settling.
	k1, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = k1.SetAccount("acc1", cfg(10000, 1000))
	id, err := k1.Reserve("acc1")
	if err != nil {
		t.Fatal(err)
	}
	_ = k1.MarkDispatched("acc1", id)
	k1.Close()

	// Second process: load state. The reservation must still be active and
	// block new dispatches (no duplicate dispatch).
	k2, err := LoadKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Reserve("acc1"); !errors.Is(err, ErrReservationActive) {
		t.Fatalf("crash must keep reservation active, got %v", err)
	}
	// Settling a dispatched reservation without a charge amount locks the
	// account for explicit reconciliation.
	if err := k2.Settle("acc1", id, nil); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("want ErrNotSettled, got %v", err)
	}
	a, _, _ := k2.Status("acc1")
	if !a.Locked {
		t.Fatal("account must be locked after uncertain settle")
	}
	if _, err := k2.Reserve("acc1"); !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("locked account must deny admission, got %v", err)
	}
	if got, _ := k2.LockedAccounts(); len(got) != 1 || got[0] != "acc1" {
		t.Fatalf("LockedAccounts = %v", got)
	}

	// Explicit human reconciliation with a known balance restores admission.
	if err := k2.Reconcile("acc1", 8500); err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Reserve("acc1"); !errors.Is(err, ErrOveragePositive) {
		t.Fatalf("reconcile leaves overage unknown and must block, got %v", err)
	}
	if err := k2.SetOverage("acc1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Reserve("acc1"); err != nil {
		t.Fatalf("admission after verified zero overage should succeed, got %v", err)
	}
	k2.Close()
}

func TestUncertainRequestStillLocked(t *testing.T) {
	// Simulate: reserve persisted, dispatch marked, process dies before
	// knowing whether the charge happened. Reload => still locked.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	k, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = k.SetAccount("acc1", cfg(5000, 1000))
	id, _ := k.Reserve("acc1")
	_ = k.MarkDispatched("acc1", id)
	k.Close()

	k2, err := LoadKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Close()
	a, _, _ := k2.Status("acc1")
	if a.Active == nil || a.Active.ID != id {
		t.Fatalf("reservation lost across restart: %+v", a)
	}
	if err := k2.Settle("acc1", id, nil); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("want ErrNotSettled, got %v", err)
	}
	a, _, _ = k2.Status("acc1")
	if !a.Locked {
		t.Fatal("must stay locked until explicit reconciliation")
	}
}

func TestSettleNeverDispatchedRefundsFully(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(10000, 1000))
	id, _ := k.Reserve("acc1")
	// Dispatch never started (phase unknown): settle with nil => full refund.
	if err := k.Settle("acc1", id, nil); err != nil {
		t.Fatal(err)
	}
	a, _, _ := k.Status("acc1")
	if *a.Available != 10000 {
		t.Fatalf("full refund expected, balance = %d", *a.Available)
	}
}

func TestAtomicSaveNoCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	k, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	_ = k.SetAccount("acc1", cfg(1000, 100))
	// State file must exist and parse after every operation.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	if k2, err := LoadKernel(path); err != nil {
		t.Fatalf("reparse failed: %v\nraw: %s", err, b)
	} else {
		k2.Close()
	}
	// No temp files left behind (the lock file is expected).
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "state.json" && e.Name() != "state.json.lock" {
			t.Fatalf("unexpected file left behind: %s", e.Name())
		}
	}
}

func TestCooldownExpiresAfterDuration(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(0, 1000))
	if _, err := k.Reserve("acc1"); !errors.Is(err, ErrBalanceBelowReserve) {
		t.Fatalf("got %v", err)
	}
	// Advance clock past cooldown (5 min at 1s ticks => advance 400 ticks).
	tick := 0
	k.nowFn = func() time.Time {
		tick++
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(tick) * time.Second)
	}
	// Top up the balance and move cooldown into the past (both via the
	// live map, since Status returns a snapshot copy).
	k.mu.Lock()
	a := k.accounts["acc1"]
	a.Available = A(5000)
	a.Overage = A(0)
	a.CooldownUntil = k.nowFn().Add(-time.Second)
	k.mu.Unlock()
	if _, err := k.Reserve("acc1"); err != nil {
		t.Fatalf("admission after cooldown expiry should succeed, got %v", err)
	}
}

func TestCorruptStateFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKernel(path); err == nil || !errors.Is(err, ErrCorruptState) {
		t.Fatalf("corrupt file must yield ErrCorruptState, got %v", err)
	}
	// Zero ReservePerDispatch persisted in state is also rejected.
	if err := os.WriteFile(path, []byte(`{"accounts":{"a":{"reservePerDispatch":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKernel(path); !errors.Is(err, ErrCorruptState) {
		t.Fatalf("state with zero reserve must be rejected, got %v", err)
	}
}

func TestConcurrentReserveSerialized(t *testing.T) {
	k, _ := newTestKernel(t)
	_ = k.SetAccount("acc1", cfg(100000, 1000))
	const n = 20
	results := make(chan error, n)
	var admitted int
	for i := 0; i < n; i++ {
		go func() {
			_, err := k.Reserve("acc1")
			results <- err
		}()
	}
	for i := 0; i < n; i++ {
		err := <-results
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrReservationActive) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if admitted != 1 {
		t.Fatalf("exactly one reservation must be admitted, got %d", admitted)
	}
}

// NewKernelNoLock is a small test helper wrapping the constructor.
func NewKernelNoLock(path string) *Kernel {
	k, err := NewKernel(path)
	if err != nil {
		panic(err)
	}
	return k
}
