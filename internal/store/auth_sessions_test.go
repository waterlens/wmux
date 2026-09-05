package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAuthSessionLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	hash := bytes.Repeat([]byte{3}, 32)
	auth, err := s.CreateAuthSession(ctx, hash, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.ID) != 32 || !bytes.Equal(auth.TokenHash, hash) {
		t.Fatalf("auth session = %+v", auth)
	}
	got, err := s.GetAuthSession(ctx, hash)
	if err != nil || got.ID != auth.ID {
		t.Fatalf("GetAuthSession = %+v, %v", got, err)
	}
	now = now.Add(10 * time.Minute)
	if err := s.TouchAuthSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAuthSession(ctx, hash)
	if err != nil || !got.LastSeenAt.Equal(now) {
		t.Fatalf("touched session = %+v, %v", got, err)
	}
	now = now.Add(time.Hour)
	if _, err := s.GetAuthSession(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session error = %v", err)
	}
	count, err := s.PurgeExpiredAuthSessions(ctx)
	if err != nil || count != 1 {
		t.Fatalf("PurgeExpiredAuthSessions = %d, %v", count, err)
	}
	if _, err := s.CreateAuthSession(ctx, []byte("short"), now.Add(time.Hour)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("short token hash error = %v", err)
	}
	if _, err := s.CreateAuthSession(ctx, hash, now.Add(-time.Hour)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("past expiry error = %v", err)
	}
}
