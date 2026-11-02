package ratelimit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmitEnforcesPerSecond(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	var calls []int64
	for i := range PerSecond {
		var wait time.Duration
		var ok bool
		calls, wait, ok = admit(calls, now)
		if !ok || wait != 0 {
			t.Fatalf("call %d refused", i)
		}
	}
	_, wait, ok := admit(calls, now)
	if ok || wait <= 0 || wait > secondWindow {
		t.Fatalf("6th call: ok=%v wait=%v", ok, wait)
	}
	if _, _, ok := admit(calls, now.Add(secondWindow+time.Millisecond)); !ok {
		t.Fatal("call refused after the window passed")
	}
}

func TestAdmitEnforcesPerMinute(t *testing.T) {
	base := time.UnixMilli(10_000_000)
	var calls []int64
	for i := range PerMinute {
		calls = append(calls, base.Add(time.Duration(i)*300*time.Millisecond).UnixMilli())
	}
	now := base.Add(31 * time.Second)
	_, wait, ok := admit(calls, now)
	if ok {
		t.Fatal("101st call admitted inside a minute")
	}
	if want := minuteWindow - 31*time.Second; wait != want {
		t.Fatalf("wait = %v, want %v", wait, want)
	}
}

func TestLimiterStateIsSharedBetweenInstances(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	var slept []time.Duration
	newLimiter := func() *Limiter {
		l := NewLimiter(store, "k")
		l.sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return context.Canceled // stop at the first required pause
		}
		return l
	}
	// Each CLI invocation is a new process, so use a new limiter per call.
	for i := range PerSecond {
		if err := newLimiter().Wait(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := newLimiter().Wait(context.Background()); err == nil || len(slept) != 1 {
		t.Fatalf("expected a pause, err=%v slept=%v", err, slept)
	}
}

func TestLimiterIsSafeUnderConcurrency(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := NewLimiter(store, "k").Wait(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var st rateState
	readJSON(filepath.Join(store.Dir, "rate-k.json"), &st)
	if len(st.Calls) != 5 {
		t.Fatalf("recorded %d calls, want 5", len(st.Calls))
	}
}

func TestLimiterFallsBackWhenStateIsUnavailable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewLimiter(Store{Dir: file}, "k").Wait(context.Background()); err != nil {
		t.Fatalf("an API call must not fail because of local state: %v", err)
	}
}

func TestStaleLockIsRecovered(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	lock := filepath.Join(store.Dir, "rate-k.json.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLimiter(store, "k").reserve(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerCountsAndRollsOverAtPragueMidnight(t *testing.T) {
	prague, err := time.LoadLocation(QuotaTimezone)
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 2, 23, 30, 0, 0, prague)
	store := Store{Dir: t.TempDir(), Now: func() time.Time { return now }}
	l := NewLedger(store, "k")
	if got := l.Status(); got.Used != 0 || got.Remaining != ReindexPerDay {
		t.Fatalf("fresh ledger: %+v", got)
	}
	if err := l.Add(3); err != nil {
		t.Fatal(err)
	}
	if got := l.Status(); got.Used != 3 || got.Remaining != ReindexPerDay-3 || !got.Estimated {
		t.Fatalf("after add: %+v", got)
	}
	now = now.Add(time.Hour)
	if got := l.Status(); got.Used != 0 || got.Day != "2026-11-03" {
		t.Fatalf("after midnight: %+v", got)
	}
}

func TestLedgerIsolatesKeys(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := NewLedger(store, "a").Add(5); err != nil {
		t.Fatal(err)
	}
	if got := NewLedger(store, "b").Status(); got.Used != 0 {
		t.Fatalf("keys share a ledger: %+v", got)
	}
}

func TestCorruptStateIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "quota-k.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: dir}
	if got := NewLedger(store, "k").Status(); got.Used != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestStateFilesNeverContainTheKey(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := NewLedger(store, "fingerprint").Add(1); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(store.Dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "secret") {
			t.Fatalf("unexpected state file %s", e.Name())
		}
		info, _ := e.Info()
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable by others: %o", e.Name(), info.Mode().Perm())
		}
	}
}
