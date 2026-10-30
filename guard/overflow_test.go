package guard

import (
	"errors"
	"math"
	"testing"
)

func TestSettlementOverBudgetLocksAndLoads(t *testing.T) {
	k, path := newTestKernel(t)
	if err := k.SetAccount("a", cfg(100, 10)); err != nil {
		t.Fatal(err)
	}
	id, err := k.Reserve("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Settle("a", id, A(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	a, _, _ := k.Status("a")
	if !a.Locked || a.Available != nil {
		t.Fatalf("unexpected %+v", a)
	}
	k.Close()
	k2, err := LoadKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Close()
	if _, err := k2.Reserve("a"); !errors.Is(err, ErrAccountLocked) {
		t.Fatal(err)
	}
}
