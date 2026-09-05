package api

import "sync"

// keyedMutex serializes work that shares a key. Session deletion, restart and
// reconnect each stop and re-create one runtime, so two of them running at once
// on the same session would race over its generation. Entries are reference
// counted, so the map only ever holds the operations currently in flight.
type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu      sync.Mutex
	holders int
}

// lock blocks until key is free and returns the matching unlock function.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = make(map[string]*keyedMutexEntry)
	}
	entry, exists := k.entries[key]
	if !exists {
		entry = &keyedMutexEntry{}
		k.entries[key] = entry
	}
	entry.holders++
	k.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.holders--
		if entry.holders == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}
