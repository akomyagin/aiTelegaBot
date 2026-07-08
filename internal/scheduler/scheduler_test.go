package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slotStore is a tiny in-memory last_digest_date store safe for -race.
type slotStore struct {
	mu   sync.Mutex
	date string
}

func (s *slotStore) get(_ context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.date, nil
}

func (s *slotStore) set(_ context.Context, date string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.date = date
	return nil
}

// newTest builds a scheduler in UTC at 09:00 with an injected fixed clock.
func newTest(t *testing.T, now time.Time, job Job, store *slotStore) *Scheduler {
	t.Helper()
	s, err := New("09:00", "UTC", job,
		WithNow(func() time.Time { return now }),
		WithSlotStore(store.get, store.set),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestCatchUp_RunsWhenNotDoneToday(t *testing.T) {
	var calls int32
	job := func(_ context.Context) error { atomic.AddInt32(&calls, 1); return nil }
	store := &slotStore{} // empty last date
	// now is after today's 09:00 slot.
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	s := newTest(t, now, job, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// Give the catch-up a moment, then cancel before the next-day timer fires.
	waitForCalls(t, &calls, 1)
	cancel()
	<-done

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("job calls = %d, want 1", got)
	}
	if store.date != "2026-07-08" {
		t.Fatalf("last date = %q, want 2026-07-08", store.date)
	}
}

func TestCatchUp_SkipsWhenAlreadyRunToday(t *testing.T) {
	var calls int32
	job := func(_ context.Context) error { atomic.AddInt32(&calls, 1); return nil }
	store := &slotStore{date: "2026-07-08"} // already ran today
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	s := newTest(t, now, job, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	// No catch-up should fire; cancel and confirm zero calls.
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("job calls = %d, want 0", got)
	}
}

func TestTick_FiresAtSlot(t *testing.T) {
	var calls int32
	job := func(_ context.Context) error { atomic.AddInt32(&calls, 1); return nil }
	store := &slotStore{}

	// now is a few ms before today's 09:00 slot so the first timer fires quickly
	// and no catch-up happens (now < todaySlot).
	slot := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	now := slot.Add(-30 * time.Millisecond)
	s := newTest(t, now, job, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()

	waitForCalls(t, &calls, 1)
	cancel()
	<-done

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("job calls = %d, want 1", got)
	}
	if store.date != "2026-07-08" {
		t.Fatalf("last date = %q, want 2026-07-08", store.date)
	}
}

// waitForCalls polls until the counter reaches want or the test times out.
func waitForCalls(t *testing.T, counter *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(counter) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d job calls, got %d", want, atomic.LoadInt32(counter))
}
