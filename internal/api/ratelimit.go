package api

import (
	"sync"
	"time"
)

// failureWindow throttles repeated login failures per client address. It is not
// an http.Handler: /api/login calls it directly so a successful password can
// clear the caller's history.
type failureWindow struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	limit   int
	window  time.Duration
	maxKeys int
}

func newFailureWindow(limit int, window time.Duration) *failureWindow {
	return &failureWindow{entries: make(map[string][]time.Time), limit: limit, window: window, maxKeys: 4096}
}

func (f *failureWindow) allowed(key string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	cutoff := now.Add(-f.window)
	values := f.entries[key]
	kept := values[:0]
	for _, value := range values {
		if value.After(cutoff) {
			kept = append(kept, value)
		}
	}
	if len(kept) == 0 {
		delete(f.entries, key)
	} else {
		f.entries[key] = kept
	}
	return len(kept) < f.limit
}

func (f *failureWindow) fail(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked(now)
	if _, exists := f.entries[key]; !exists && len(f.entries) >= f.maxKeys {
		// The map is full of sources that failed inside the window. Dropping one
		// of them would almost always drop a real user, since the flooder's own
		// entry is the freshest, so the newcomer goes uncounted this round.
		return
	}
	f.entries[key] = append(f.entries[key], now)
}

func (f *failureWindow) clear(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, key)
}

func (f *failureWindow) pruneLocked(now time.Time) {
	cutoff := now.Add(-f.window)
	for key, values := range f.entries {
		kept := values[:0]
		for _, value := range values {
			if value.After(cutoff) {
				kept = append(kept, value)
			}
		}
		if len(kept) == 0 {
			delete(f.entries, key)
		} else {
			f.entries[key] = kept
		}
	}
}
