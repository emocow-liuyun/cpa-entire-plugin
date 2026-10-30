// Package guard implements a standalone spend-admission kernel for the CPA
// Entire plugin. It is a pure-Go, stdlib-only library that enforces:
//
//   - Single-account serialized admission (one in-flight reservation per
//     account), and a single live kernel instance per state file (file lock).
//   - Cooling when balance is zero or below the reserve amount.
//   - Fail-closed behavior for unknown balance, unknown auto-recharge flag,
//     and any non-zero overage.
//   - Auto-recharge ON blocks admission unconditionally: relying on a top-up
//     means the dispatch depends on funds not yet present.
//   - Conservative high-water-mark reservation with per-charge settlement.
//   - Atomic persistence; a persistence failure poisons the kernel so no
//     further dispatch can be admitted from an unrecorded state.
//   - Crash after persisting a reservation keeps the reservation locked until
//     explicit human reconciliation (no duplicate dispatch).
//
// The kernel does NOT guarantee absence of overspend: external campaign
// activity, network delays, and charge reporting lag can break any such
// guarantee. It only makes the local admission decision conservative.
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Amount is a spend amount in the account's minor unit (e.g. cents).
// All arithmetic is integer to avoid float drift.
type Amount int64

// Account models one ad account's admission state.
type Account struct {
	// Balance is the last known available balance in minor units.
	// Available == nil means the balance is unknown/unavailable (fail-closed).
	Available *Amount `json:"available,omitempty"`

	// AutoRecharge is the auto top-up switch. Tri-state:
	//
	//   nil   (unknown)  -> fail-closed, admission denied.
	//   true (enabled)   -> admission denied unconditionally: any dispatch
	//                       relying on a future top-up is out of scope.
	//   false (disabled) -> normal admission rules apply.
	AutoRecharge *bool `json:"autoRecharge,omitempty"`

	// Overage is the last observed projected overage. Any value > 0 blocks
	// admission unconditionally (fail-closed), regardless of AutoRecharge.
	// nil (never observed) blocks admission.
	Overage *Amount `json:"overage,omitempty"`

	// ReservePerDispatch is the conservative high-water-mark amount reserved
	// for every dispatch against this account. Must be > 0.
	ReservePerDispatch Amount `json:"reservePerDispatch"`

	// RunsPaused, when known (non-nil) and true, blocks all admission.
	// nil (unknown) is fail-closed and blocks admission as well.
	RunsPaused *bool `json:"runsPaused,omitempty"`

	// CooldownUntil, if non-zero, blocks admission until that instant.
	CooldownUntil time.Time `json:"cooldownUntil,omitempty"`

	// Active is the in-flight reservation, if any. Serialized: at most one.
	Active *Reservation `json:"active,omitempty"`

	// Locked marks the account as requiring explicit human reconciliation
	// (e.g. crash after an uncertain request). While locked, admission is
	// denied.
	Locked bool `json:"locked,omitempty"`

	// LockedReason explains why reconciliation is needed.
	LockedReason string `json:"lockedReason,omitempty"`
}

// Reservation is one in-flight dispatch hold.
type Reservation struct {
	ID        string    `json:"id"`
	Amount    Amount    `json:"amount"`
	CreatedAt time.Time `json:"createdAt"`
	// Settled records that the real charge was reported and settled.
	Settled bool      `json:"settled,omitempty"`
	Charged *Amount   `json:"charged,omitempty"`
	Phase   *JobPhase `json:"phase,omitempty"`
}

// JobPhase tracks whether the dispatch actually started, used by
// Reconcile to decide whether the charge may already have happened.
type JobPhase string

const (
	PhaseUnknown    JobPhase = "unknown"
	PhaseDispatched JobPhase = "dispatched"
)

// Kernel errors.
var (
	ErrAccountLocked       = errors.New("guard: account locked, explicit reconciliation required")
	ErrReservationActive   = errors.New("guard: another reservation is active for this account")
	ErrBalanceUnknown      = errors.New("guard: balance unknown or unavailable (fail-closed)")
	ErrBalanceBelowReserve = errors.New("guard: balance zero or below reserve amount (cooldown)")
	ErrAutoRechargeEnabled = errors.New("guard: auto-recharge enabled, admission blocked (fail-closed)")
	ErrAutoRechargeUnknown = errors.New("guard: auto-recharge flag unknown, admission blocked (fail-closed)")
	ErrOveragePositive     = errors.New("guard: non-zero overage observed, admission blocked (fail-closed)")
	ErrRunsPaused          = errors.New("guard: runs paused or pause state unknown, admission blocked (fail-closed)")
	ErrNoReservation       = errors.New("guard: no active reservation")
	ErrNotSettled          = errors.New("guard: reservation not settled")
	ErrCooldown            = errors.New("guard: account in cooldown")
	ErrBadInput            = errors.New("guard: invalid input")
	ErrPersistFailed       = errors.New("guard: state persistence failed, kernel poisoned (fail-closed)")
	ErrInstanceActive      = errors.New("guard: another kernel instance already holds the state file")
	ErrClosed              = errors.New("guard: kernel closed")
	ErrCorruptState        = errors.New("guard: corrupt or incompatible state file")
	// Deprecated alias kept for source compatibility with older callers.
	ErrAutoRechargeOverage = ErrOveragePositive
)

// Kernel is the admission kernel. Create with NewKernel or LoadKernel.
// Exactly one Kernel may hold a given state file at a time; a second
// instance is rejected with ErrInstanceActive. Release the file lock with
// Close.
type Kernel struct {
	mu       sync.Mutex
	path     string
	lockPath string
	lockFD   int
	nowFn    func() time.Time
	idFn     func() string
	// poisoned is set when any persistence attempt fails. From that point
	// on, admission and mutations are denied: an unrecorded state may hide
	// an unaccounted spend, so further runs must not proceed.
	poisoned bool
	closed   bool
	// failSaveFn, when set (tests), simulates persistence failure.
	failSaveFn func() error
	accounts   map[string]*Account
}

// NewKernel acquires the exclusive lock and loads existing state, or creates
// an empty in-memory state if none exists. Invalid or unresolved state is
// rejected. A second live instance returns ErrInstanceActive.
func NewKernel(path string) (*Kernel, error) {
	k := &Kernel{
		path:     path,
		lockPath: path + ".lock",
		lockFD:   -1,
		nowFn:    time.Now,
		idFn:     newID,
		accounts: map[string]*Account{},
	}
	if err := k.acquireLock(); err != nil {
		return nil, err
	}
	if err := k.load(); err != nil {
		k.releaseLock()
		return nil, err
	}
	return k, nil
}

// LoadKernel restores a kernel from path. Missing file yields an empty
// kernel. Unknown fields in the state file are rejected (fail-closed): a
// state written by a different schema version must not be silently
// reinterpreted. It also enforces the single-instance file lock.
func LoadKernel(path string) (*Kernel, error) { return NewKernel(path) }

// load is called only while the instance lock is held.
func (k *Kernel) load() error {
	// A pre-rename crash can leave a newer reservation in a temporary file.
	// Never guess which image is authoritative; require manual reconciliation.
	entries, err := os.ReadDir(filepath.Dir(k.path))
	if err != nil {
		return err
	}
	prefix := ".guard-" + filepath.Base(k.path) + "-tmp-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) || strings.HasPrefix(entry.Name(), ".guard-tmp-") {
			return fmt.Errorf("%w: unresolved temporary state %q", ErrCorruptState, entry.Name())
		}
	}
	b, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var stored struct {
		Accounts map[string]*Account `json:"accounts"`
	}
	if err := dec.Decode(&stored); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%w: trailing content", ErrCorruptState)
	}
	if stored.Accounts == nil {
		return fmt.Errorf("%w: missing or null accounts", ErrCorruptState)
	}
	for id, a := range stored.Accounts {
		if id == "" || a == nil {
			return fmt.Errorf("%w: invalid account %q", ErrCorruptState, id)
		}
		if err := validateAccount(*a); err != nil {
			return fmt.Errorf("%w: account %q: %v", ErrCorruptState, id, err)
		}
		if a.Active != nil {
			r := a.Active
			if r.ID == "" || r.Amount <= 0 || r.CreatedAt.IsZero() || r.Settled || r.Charged != nil || (r.Phase != nil && *r.Phase != PhaseUnknown && *r.Phase != PhaseDispatched) {
				return fmt.Errorf("%w: invalid active reservation for %q", ErrCorruptState, id)
			}
		}
	}
	k.accounts = stored.Accounts
	return nil
}

// acquireLock takes an exclusive, non-blocking lock on the lock file.
func (k *Kernel) acquireLock() error {
	fd, err := syscall.Open(k.lockPath, syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("guard: open lock file: %w", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return fmt.Errorf("%w: %v", ErrInstanceActive, err)
	}
	k.lockFD = fd
	return nil
}

func (k *Kernel) releaseLock() {
	if k.lockFD >= 0 {
		syscall.Flock(k.lockFD, syscall.LOCK_UN)
		syscall.Close(k.lockFD)
		k.lockFD = -1
	}
}

// Close releases the state-file lock. Further operations on the kernel are
// invalid after Close.
func (k *Kernel) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil
	}
	k.closed = true
	k.releaseLock()
	return nil
}

// save atomically writes state: temp file in same dir + rename. On failure
// the kernel is poisoned: ErrPersistFailed is returned and every subsequent
// mutating/admission call is denied until the process is restarted on a
// writable path, because in-memory state has diverged from disk.
func (k *Kernel) save() error {
	if k.failSaveFn != nil {
		if err := k.failSaveFn(); err != nil {
			k.poisoned = true
			return fmt.Errorf("%w: %v", ErrPersistFailed, err)
		}
	}
	b, err := json.MarshalIndent(struct {
		Accounts map[string]*Account `json:"accounts"`
	}{k.accounts}, "", "  ")
	if err != nil {
		k.poisoned = true
		return fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	dir := filepath.Dir(k.path)
	tmp, err := os.CreateTemp(dir, ".guard-"+filepath.Base(k.path)+"-tmp-*")
	if err != nil {
		k.poisoned = true
		return fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		k.poisoned = true
		return fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		k.poisoned = true
		return fmt.Errorf("%w: sync state: %v", ErrPersistFailed, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		k.poisoned = true
		return fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	if err := os.Rename(tmpName, k.path); err != nil {
		os.Remove(tmpName)
		k.poisoned = true
		return fmt.Errorf("%w: %v", ErrPersistFailed, err)
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		closeErr := d.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		k.poisoned = true
		return fmt.Errorf("%w: sync directory: %v", ErrPersistFailed, err)
	}
	return nil
}

// checkPoisoned returns ErrPersistFailed if the kernel already failed to
// persist state and therefore must not run any further.
func (k *Kernel) checkPoisoned() error {
	if k.closed {
		return ErrClosed
	}
	if k.poisoned {
		return ErrPersistFailed
	}
	return nil
}

// validate checks the settable fields of a Account configuration.
func validateAccount(a Account) error {
	if a.ReservePerDispatch <= 0 {
		return fmt.Errorf("%w: ReservePerDispatch must be > 0", ErrBadInput)
	}
	if a.Available != nil && *a.Available < 0 {
		return fmt.Errorf("%w: Available must not be negative", ErrBadInput)
	}
	if a.Overage != nil && *a.Overage < 0 {
		return fmt.Errorf("%w: Overage must not be negative", ErrBadInput)
	}
	return nil
}

// cloneAccount isolates every pointer, including nested reservation fields.
func cloneAccount(a Account) Account {
	if a.Available != nil {
		v := *a.Available
		a.Available = &v
	}
	if a.AutoRecharge != nil {
		v := *a.AutoRecharge
		a.AutoRecharge = &v
	}
	if a.Overage != nil {
		v := *a.Overage
		a.Overage = &v
	}
	if a.RunsPaused != nil {
		v := *a.RunsPaused
		a.RunsPaused = &v
	}
	if a.Active != nil {
		r := *a.Active
		if r.Charged != nil {
			v := *r.Charged
			r.Charged = &v
		}
		if r.Phase != nil {
			v := *r.Phase
			r.Phase = &v
		}
		a.Active = &r
	}
	return a
}

// SetAccount configures an account's admission parameters. ReservePerDispatch
// must be > 0. If an un-settled reservation is active, the previously
// recorded local budget (Available and ReservePerDispatch) is preserved and
// must not be overwritten by the new configuration.
func (k *Kernel) SetAccount(id string, a Account) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return err
	}
	if id == "" {
		return ErrBadInput
	}
	if err := validateAccount(a); err != nil {
		return err
	}

	prev := k.accounts[id]
	merged := cloneAccount(a)
	if prev != nil {
		// Preserve operational state across reconfiguration.
		merged.Active = prev.Active
		merged.Locked = prev.Locked
		merged.LockedReason = prev.LockedReason
		merged.CooldownUntil = prev.CooldownUntil
		if prev.Active != nil {
			// An un-settled reservation exists: its budget snapshot must
			// survive reconfiguration untouched.
			merged.Available = prev.Available
			merged.ReservePerDispatch = prev.ReservePerDispatch
		}
	}
	k.accounts[id] = &merged
	return k.save()
}

// SetOverage records the latest projected overage. A value > 0 blocks all
// subsequent admission until it is cleared by an observation or Reconcile.
func (k *Kernel) SetOverage(accountID string, overage Amount) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return err
	}
	if overage < 0 {
		return fmt.Errorf("%w: Overage must not be negative", ErrBadInput)
	}

	a, ok := k.accounts[accountID]
	if !ok {
		return ErrBadInput
	}
	o := overage
	a.Overage = &o
	return k.save()
}

// admissionBlocked reports whether a policy field blocks admission in the
// fail-closed sense, with the specific error.
func admissionBlocked(a *Account) error {
	// Auto-recharge: enabled => block unconditionally; unknown => fail-closed.
	switch {
	case a.AutoRecharge == nil:
		return ErrAutoRechargeUnknown
	case *a.AutoRecharge:
		return ErrAutoRechargeEnabled
	}
	// Non-zero overage blocks regardless of the auto-recharge switch.
	if a.Overage == nil || *a.Overage > 0 {
		return ErrOveragePositive
	}
	// Runs paused: paused or unknown => block (fail-closed).
	if a.RunsPaused == nil || *a.RunsPaused {
		return ErrRunsPaused
	}
	return nil
}

// Reserve attempts to admit one dispatch on the given account using the
// conservative high-water-mark reserve amount. It returns the reservation id.
//
// Rules enforced in order (all fail-closed):
//  1. Kernel poisoned by a past persistence failure -> ErrPersistFailed.
//  2. Locked account -> ErrAccountLocked.
//  3. Active reservation -> ErrReservationActive (serialization).
//  4. Cooldown active -> ErrCooldown.
//  5. AutoRecharge enabled or unknown -> ErrAutoRechargeEnabled /
//     ErrAutoRechargeUnknown.
//  6. Overage > 0 -> ErrOveragePositive.
//  7. RunsPaused true or unknown -> ErrRunsPaused.
//  8. Balance unknown -> ErrBalanceUnknown.
//  9. Balance < reserve (incl. zero) -> set cooldown and
//     ErrBalanceBelowReserve.
func (k *Kernel) Reserve(accountID string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return "", err
	}
	if accountID == "" {
		return "", ErrBadInput
	}

	a, ok := k.accounts[accountID]
	if !ok {
		return "", ErrBadInput
	}
	now := k.nowFn()
	if a.Locked {
		return "", ErrAccountLocked
	}
	if a.Active != nil {
		return "", ErrReservationActive
	}
	if !a.CooldownUntil.IsZero() && now.Before(a.CooldownUntil) {
		return "", ErrCooldown
	}
	if err := admissionBlocked(a); err != nil {
		return "", err
	}
	if a.Available == nil {
		return "", ErrBalanceUnknown
	}
	bal := *a.Available
	if bal < a.ReservePerDispatch {
		// Enter cooldown: zero or below-reserve balance.
		a.CooldownUntil = now.Add(cooldownDuration)
		// A below-reserve balance implies a projected overage.
		ov := a.ReservePerDispatch - bal
		a.Overage = &ov
		if err := k.save(); err != nil {
			return "", err
		}
		if bal <= 0 {
			return "", fmt.Errorf("%w: balance %d", ErrBalanceBelowReserve, bal)
		}
		return "", fmt.Errorf("%w: balance %d < reserve %d",
			ErrBalanceBelowReserve, bal, a.ReservePerDispatch)
	}
	res := &Reservation{
		ID:        k.idFn(),
		Amount:    a.ReservePerDispatch,
		CreatedAt: now,
		Phase:     func() *JobPhase { p := PhaseUnknown; return &p }(),
	}
	a.Active = res
	// High-water-mark: optimistically reduce observed balance so a concurrent
	// external read does not double-spend; the real charge settles later.
	nb := bal - a.ReservePerDispatch
	a.Available = &nb
	if err := k.save(); err != nil {
		// A rename may already have occurred; the poisoned kernel cannot
		// determine which image is durable. Retain the conservative hold.
		return "", err
	}
	return res.ID, nil
}

// MarkDispatched records that the reserved dispatch actually started. After
// this point a crash leaves the reservation locked (uncertain outcome).
func (k *Kernel) MarkDispatched(accountID, reservationID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return err
	}
	a, ok := k.accounts[accountID]
	if !ok {
		return ErrBadInput
	}
	if a.Active == nil || a.Active.ID != reservationID {
		return ErrNoReservation
	}
	p := PhaseDispatched
	a.Active.Phase = &p
	return k.save()
}

// Settle reports the actual charge for a reservation and clears it. A
// negative charge is rejected. If the dispatch never happened (Phase
// unknown), the full reserve is refunded. The account is locked before save
// when the dispatch had started but the charge amount is unknown, forcing
// explicit reconciliation instead of guessing.
func (k *Kernel) Settle(accountID, reservationID string, charged *Amount) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return err
	}
	if charged != nil && *charged < 0 {
		return fmt.Errorf("%w: charged amount must not be negative", ErrBadInput)
	}

	a, ok := k.accounts[accountID]
	if !ok {
		return ErrBadInput
	}
	if a.Active == nil || a.Active.ID != reservationID {
		return ErrNoReservation
	}
	res := a.Active
	if res.Phase != nil && *res.Phase == PhaseDispatched && charged == nil {
		// Uncertain outcome: lock for human reconciliation rather than
		// silently refunding or double-charging.
		a.Locked = true
		a.LockedReason = fmt.Sprintf("settle without charged amount on dispatched reservation %s", res.ID)
		if err := k.save(); err != nil {
			return err
		}
		return ErrNotSettled
	}
	charge := Amount(0)
	if charged != nil {
		charge = *charged
	}
	// Adjust: refund reserve minus actual charge.
	refund := res.Amount - charge
	bal := Amount(0)
	if a.Available != nil {
		bal = *a.Available
	}
	// Avoid signed overflow and retain a reconciliation lock if the actual
	// charge exceeds the observed budget. Unknown overage remains blocking.
	if refund > 0 && bal > Amount(^uint64(0)>>1)-refund {
		a.Locked = true
		a.LockedReason = "settlement balance overflow; reconciliation required"
		a.Available = nil
	} else {
		nb := bal + refund
		if nb < 0 {
			a.Locked = true
			a.LockedReason = "charge exceeded observed balance; reconciliation required"
			a.Available = nil
		} else {
			a.Available = &nb
		}
	}
	a.Active = nil
	// Settlement resolves the previously observed overage projection.
	a.Overage = nil
	return k.save()
}

// Reconcile is the explicit human reconciliation entry point. It resolves a
// locked account after an operator has determined the true outcome of the
// in-flight (or lost) reservation: whether the dispatch happened and what it
// actually charged.
func (k *Kernel) Reconcile(accountID string, newBalance Amount) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.checkPoisoned(); err != nil {
		return err
	}
	if newBalance < 0 {
		return fmt.Errorf("%w: reconciled balance must not be negative", ErrBadInput)
	}

	a, ok := k.accounts[accountID]
	if !ok {
		return ErrBadInput
	}
	a.Locked = false
	a.LockedReason = ""
	a.Available = &newBalance
	a.Active = nil
	a.Overage = nil
	a.CooldownUntil = time.Time{}
	return k.save()
}

// Status returns a snapshot of an account.
func (k *Kernel) Status(accountID string) (Account, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return Account{}, false, ErrClosed
	}
	a, ok := k.accounts[accountID]
	if !ok {
		return Account{}, false, nil
	}
	return cloneAccount(*a), true, nil
}

// LockedAccounts lists accounts needing reconciliation, sorted by id for
// deterministic output.
func (k *Kernel) LockedAccounts() ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil, ErrClosed
	}
	var out []string
	for id, a := range k.accounts {
		if a.Locked {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// cooldownDuration is how long an account cools after a below-reserve read.
const cooldownDuration = 5 * time.Minute

var idCounter atomic.Int64

// newID generates a reservation id. Deterministic in tests via idFn override;
// in production uses crypto/rand-backed uniqueness through time+counter.
func newID() string {
	return fmt.Sprintf("rsv-%d-%06d", time.Now().UnixNano(), idCounter.Add(1))
}
