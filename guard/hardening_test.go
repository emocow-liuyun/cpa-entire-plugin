package guard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNewLoadsExistingReservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	k, err := NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = k.SetAccount("a", cfg(5000, 1000)); err != nil {
		t.Fatal(err)
	}
	id, err := k.Reserve("a")
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	k, err = NewKernel(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	a, ok, err := k.Status("a")
	if err != nil || !ok || a.Active == nil || a.Active.ID != id {
		t.Fatalf("reservation lost: %+v %v", a, err)
	}
	if _, err = k.Reserve("a"); !errors.Is(err, ErrReservationActive) {
		t.Fatal(err)
	}
}

func TestClosedPublicMethods(t *testing.T) {
	k, _ := newTestKernel(t)
	if err := k.SetAccount("a", cfg(100, 10)); err != nil {
		t.Fatal(err)
	}
	k.Close()
	checks := []struct {
		name string
		fn   func() error
	}{
		{"set", func() error { return k.SetAccount("", Account{}) }},
		{"overage", func() error { return k.SetOverage("", -1) }},
		{"reserve", func() error { _, err := k.Reserve(""); return err }},
		{"dispatch", func() error { return k.MarkDispatched("", "") }},
		{"settle", func() error { return k.Settle("", "", A(-1)) }},
		{"reconcile", func() error { return k.Reconcile("", -1) }},
		{"status", func() error { _, _, err := k.Status("a"); return err }},
		{"locked", func() error { _, err := k.LockedAccounts(); return err }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(); !errors.Is(err, ErrClosed) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotsAndInputAreDeepCopied(t *testing.T) {
	k, _ := newTestKernel(t)
	a := cfg(5000, 1000)
	if err := k.SetAccount("a", a); err != nil {
		t.Fatal(err)
	}
	*a.Available = 1
	*a.Overage = 1
	*a.AutoRecharge = true
	*a.RunsPaused = true
	id, err := k.Reserve("a")
	if err != nil {
		t.Fatal(err)
	}
	snap, ok, err := k.Status("a")
	if err != nil || !ok {
		t.Fatal(err)
	}
	*snap.Available = 0
	*snap.Overage = 22
	*snap.AutoRecharge = true
	*snap.RunsPaused = true
	snap.Active.ID = "modified"
	*snap.Active.Phase = PhaseDispatched
	again, _, _ := k.Status("a")
	if *again.Available != 4000 || *again.Overage != 0 || *again.AutoRecharge || *again.RunsPaused || again.Active.ID != id || *again.Active.Phase != PhaseUnknown {
		t.Fatalf("snapshot mutated state %+v", again)
	}
}

func TestConcurrentSnapshotsAreIsolated(t *testing.T) {
	k, _ := newTestKernel(t)
	if err := k.SetAccount("a", cfg(5000, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Reserve("a"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				a, ok, err := k.Status("a")
				if err == nil && ok {
					*a.Available = 1
					*a.Overage = 2
					*a.Active.Phase = PhaseDispatched
					a.Active.ID = "changed"
				}
				_ = k.SetOverage("a", 0)
			}
		}()
	}
	wg.Wait()
	a, _, _ := k.Status("a")
	if *a.Available != 4000 || a.Active.ID == "changed" || *a.Active.Phase != PhaseUnknown {
		t.Fatalf("state mutated %+v", a)
	}
}

func TestCorruptStatesNeverOverwritten(t *testing.T) {
	cases := []string{`null`, `{}`, `{"accounts":null}`, `{"accounts":{}} {}`, `{"accounts":{"":{"reservePerDispatch":1}}}`, `{"accounts":{"a":{"available":-1,"reservePerDispatch":1}}}`, `{"accounts":{"a":{"overage":-1,"reservePerDispatch":1}}}`, `{"accounts":{"a":{"reservePerDispatch":1,"active":{"id":"","amount":1}}}}`, `{"accounts":{"a":{"reservePerDispatch":1,"active":{"id":"x","amount":1,"createdAt":"2026-01-01T00:00:00Z","phase":"invalid"}}}}`}
	for i, raw := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func(string) (*Kernel, error){NewKernel, LoadKernel} {
				k, err := open(p)
				if k != nil {
					k.Close()
				}
				if !errors.Is(err, ErrCorruptState) {
					t.Fatalf("%s got %v", raw, err)
				}
			}
			b, _ := os.ReadFile(p)
			if string(b) != raw {
				t.Fatal("state overwritten")
			}
		})
	}
}

func TestCrashTemporaryFileBlocksLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	tmp := filepath.Join(filepath.Dir(p), ".guard-state.json-tmp-interrupted")
	if err := os.WriteFile(tmp, []byte(`{"accounts":`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(string) (*Kernel, error){NewKernel, LoadKernel} {
		k, err := open(p)
		if k != nil {
			k.Close()
		}
		if !errors.Is(err, ErrCorruptState) {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("unresolved temp must be retained", err)
	}
}

func TestCrossProcessLockOnFilesystem(t *testing.T) {
	if p := os.Getenv("GUARD_LOCK_CHILD"); p != "" {
		k, err := NewKernel(p)
		if k != nil {
			k.Close()
		}
		if !errors.Is(err, ErrInstanceActive) {
			fmt.Fprintln(os.Stderr, "child got", err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	root := os.Getenv("GUARD_LOCK_TEST_DIR")
	if root == "" {
		root = t.TempDir()
	}
	dir, err := os.MkdirTemp(root, "guard-lock-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "state.json")
	k, err := NewKernel(p)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessLockOnFilesystem$")
	cmd.Env = append(os.Environ(), "GUARD_LOCK_CHILD="+p)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cross-process lock failed: %v %s", err, strings.TrimSpace(string(out)))
	}
}
