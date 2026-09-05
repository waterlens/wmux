package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSingleUserSetupAndPassword(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	setup, err := s.IsSetup(ctx)
	if err != nil || setup {
		t.Fatalf("initial IsSetup = %v, %v", setup, err)
	}
	if _, err := s.GetUser(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("initial GetUser error = %v", err)
	}
	if err := s.Setup(ctx, "owner", "hash-one"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := s.Setup(ctx, "other", "hash-two"); !errors.Is(err, ErrAlreadySetup) {
		t.Fatalf("second Setup error = %v", err)
	}
	user, err := s.GetUserByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "owner" || user.PasswordHash != "hash-one" || !user.CreatedAt.Equal(now) {
		t.Fatalf("user = %+v", user)
	}
	if _, err := s.GetUserByUsername(ctx, "OWNER"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong username error = %v", err)
	}
	now = now.Add(time.Hour)
	if err := s.UpdatePassword(ctx, "hash-three"); err != nil {
		t.Fatal(err)
	}
	user, err = s.GetUser(ctx)
	if err != nil || user.PasswordHash != "hash-three" || !user.UpdatedAt.Equal(now) {
		t.Fatalf("updated user = %+v, %v", user, err)
	}
}

func TestSetupAllowsExactlyOneConcurrentCaller(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	const workers = 8
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.Setup(context.Background(), "owner", "hash")
		}()
	}
	wg.Wait()
	close(results)
	var successes, already int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadySetup):
			already++
		default:
			t.Fatalf("unexpected Setup error: %v", err)
		}
	}
	if successes != 1 || already != workers-1 {
		t.Fatalf("successes=%d already=%d", successes, already)
	}
}
