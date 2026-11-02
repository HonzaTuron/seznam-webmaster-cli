// Package ratelimit enforces Seznam's published limits across processes.
//
// Agents usually call a CLI in a loop, starting a new process per URL, so an
// in-memory limiter would protect nothing. Call timestamps live in a small
// state file keyed by a fingerprint of the API key (never the key itself).
//
// State is best effort: if it cannot be read or written, szwm falls back to an
// in-process limiter rather than failing the API call.
package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Published limits, per user and per site.
const (
	PerSecond       = 5
	PerMinute       = 100
	ReindexPerDay   = 500
	QuotaTimezone   = "Europe/Prague"
	secondWindow    = 1050 * time.Millisecond
	minuteWindow    = 60500 * time.Millisecond
	lockStaleAfter  = 5 * time.Second
	lockWaitTimeout = 2 * time.Second
)

// Store is the on-disk state location.
type Store struct {
	Dir string
	Now func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// withLock runs fn while holding a lock file. Lock files instead of flock keep
// the behaviour identical on Linux, macOS and Windows.
func (s Store) withLock(name string, fn func(path string) error) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.Dir, name)
	lock := path + ".lock"
	deadline := time.Now().Add(lockWaitTimeout)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for state lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer os.Remove(lock)
	return fn(path)
}

func readJSON(path string, v any) {
	if data, err := os.ReadFile(path); err == nil {
		// A corrupt state file is treated as empty: it holds only estimates.
		_ = json.Unmarshal(data, v)
	}
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Limiter is a sliding-window limiter shared between processes.
type Limiter struct {
	store Store
	name  string
	sleep func(context.Context, time.Duration) error

	mu     sync.Mutex
	memory []int64
}

// NewLimiter returns a limiter for one key fingerprint.
func NewLimiter(store Store, keyID string) *Limiter {
	return &Limiter{store: store, name: "rate-" + keyID + ".json", sleep: sleepCtx}
}

type rateState struct {
	Calls []int64 `json:"calls"`
}

// Wait blocks until one more call fits both windows, then records it.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		wait, err := l.reserve()
		if err != nil {
			wait = l.reserveMemory()
		}
		if wait <= 0 {
			return nil
		}
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (l *Limiter) reserve() (time.Duration, error) {
	var wait time.Duration
	err := l.store.withLock(l.name, func(path string) error {
		var st rateState
		readJSON(path, &st)
		var ok bool
		st.Calls, wait, ok = admit(st.Calls, l.store.now())
		if !ok {
			return nil
		}
		return writeJSON(path, st)
	})
	return wait, err
}

func (l *Limiter) reserveMemory() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	calls, wait, _ := admit(l.memory, l.store.now())
	l.memory = calls
	return wait
}

// admit prunes calls and either records now or says how long to wait.
func admit(calls []int64, now time.Time) ([]int64, time.Duration, bool) {
	nowMS := now.UnixMilli()
	kept := calls[:0:0]
	for _, c := range calls {
		if c > nowMS-minuteWindow.Milliseconds() && c <= nowMS {
			kept = append(kept, c)
		}
	}
	var wait time.Duration
	if n := len(kept); n >= PerMinute {
		wait = time.Duration(kept[n-PerMinute]+minuteWindow.Milliseconds()-nowMS) * time.Millisecond
	}
	recent := 0
	for _, c := range kept {
		if c > nowMS-secondWindow.Milliseconds() {
			recent++
		}
	}
	if recent >= PerSecond {
		w := time.Duration(kept[len(kept)-PerSecond]+secondWindow.Milliseconds()-nowMS) * time.Millisecond
		wait = max(wait, w)
	}
	if wait > 0 {
		return kept, wait, false
	}
	return append(kept, nowMS), 0, true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Quota is a snapshot of the local reindex ledger.
//
// The API exposes no counter, so this is an estimate of calls made from this
// machine. Seznam does not document when "a day" starts; szwm assumes midnight
// in Europe/Prague.
type Quota struct {
	Day       string `json:"day"`
	Timezone  string `json:"timezone"`
	Limit     int    `json:"limit"`
	Used      int    `json:"used_estimate"`
	Remaining int    `json:"remaining_estimate"`
	Estimated bool   `json:"estimated"`
}

// Ledger counts successful reindex requests per day.
type Ledger struct {
	store Store
	name  string
}

// NewLedger returns the ledger for one key fingerprint.
func NewLedger(store Store, keyID string) *Ledger {
	return &Ledger{store: store, name: "quota-" + keyID + ".json"}
}

type quotaState struct {
	Days map[string]int `json:"days"`
}

func (l *Ledger) day() string {
	loc, err := time.LoadLocation(QuotaTimezone)
	if err != nil {
		loc = time.UTC
	}
	return l.store.now().In(loc).Format("2006-01-02")
}

func snapshot(day string, used int) Quota {
	return Quota{
		Day: day, Timezone: QuotaTimezone, Limit: ReindexPerDay, Used: used,
		Remaining: max(ReindexPerDay-used, 0), Estimated: true,
	}
}

// Status returns today's estimate.
func (l *Ledger) Status() Quota {
	day := l.day()
	var st quotaState
	readJSON(filepath.Join(l.store.Dir, l.name), &st)
	return snapshot(day, st.Days[day])
}

// Add records n submitted requests and drops older days.
func (l *Ledger) Add(n int) error {
	day := l.day()
	return l.store.withLock(l.name, func(path string) error {
		var st quotaState
		readJSON(path, &st)
		st.Days = map[string]int{day: st.Days[day] + n}
		return writeJSON(path, st)
	})
}
