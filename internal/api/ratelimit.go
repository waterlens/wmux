package api

import (
	"sync"
	"time"
)

// loginLockout blocks /api/login for everyone after too many wrong passwords.
//
// wmux has a single account, so a per-address limit would only slow down an
// attacker with many addresses; the trade-off of a global lock is that anyone
// who fails `limit` times locks new logins for `duration`. Sessions that are
// already signed in keep working because requests are validated by token, and
// the state lives in memory, so restarting wmux lifts the lock.
type loginLockout struct {
	mu           sync.Mutex
	limit        int
	duration     time.Duration
	failures     int
	lastFailure  time.Time
	blockedUntil time.Time
}

func newLoginLockout(limit int, duration time.Duration) *loginLockout {
	return &loginLockout{limit: limit, duration: duration}
}

// remaining reports how much longer logins stay blocked; zero means attempts
// are allowed. An expired lock resets the failure count so the next round
// starts from scratch.
func (l *loginLockout) remaining(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.blockedUntil) {
		return l.blockedUntil.Sub(now)
	}
	if !l.blockedUntil.IsZero() {
		l.blockedUntil = time.Time{}
		l.failures = 0
	}
	return 0
}

// fail records a wrong password. Failures older than `duration` are forgotten;
// the failure that reaches the limit engages the lock and reports true.
func (l *loginLockout) fail(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures > 0 && now.Sub(l.lastFailure) > l.duration {
		l.failures = 0
	}
	l.failures++
	l.lastFailure = now
	if l.failures < l.limit {
		return false
	}
	l.failures = 0
	l.blockedUntil = now.Add(l.duration)
	return true
}

// clear forgets earlier failures after a successful login.
func (l *loginLockout) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = 0
	l.blockedUntil = time.Time{}
}
