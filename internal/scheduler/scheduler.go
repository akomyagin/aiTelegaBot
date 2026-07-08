// Package scheduler runs the daily digest job at a configured local time.
//
// MVP uses stdlib time: it computes the next daily slot in the given timezone
// (time.LoadLocation) and waits via time.Timer with cancellation through
// context (never a blocking time.Sleep without ctx). Slot idempotency across
// restarts is backed by SQLite meta ("last_digest_date"): on start it checks
// whether today's slot was already run, so a missed slot is caught up and a
// completed one is not duplicated. See docs/TECHNICAL_PLAN.md §5, SKILL.md §5.
//
// Этап 4: real time-based scheduler with catch-up and slot idempotency.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// slotDateLayout is the date format used as the idempotency key for a slot.
const slotDateLayout = "2006-01-02"

// Job is the unit of work run at each slot (the digest pipeline).
type Job func(ctx context.Context) error

// Scheduler fires Job once per day at hour:min in loc.
type Scheduler struct {
	hour    int
	min     int
	loc     *time.Location
	job     Job
	now     func() time.Time                          // injectable clock; default time.Now
	getSlot func(ctx context.Context) (string, error) // reads meta "last_digest_date"
	setSlot func(ctx context.Context, date string) error
	log     *slog.Logger
}

// Option configures the Scheduler.
type Option func(*Scheduler)

// WithNow injects a custom clock (for tests).
func WithNow(fn func() time.Time) Option {
	return func(s *Scheduler) { s.now = fn }
}

// WithSlotStore wires the last-run-date persistence for slot idempotency.
func WithSlotStore(get func(ctx context.Context) (string, error), set func(ctx context.Context, date string) error) Option {
	return func(s *Scheduler) {
		s.getSlot = get
		s.setSlot = set
	}
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(log *slog.Logger) Option {
	return func(s *Scheduler) { s.log = log }
}

// New builds a Scheduler. digestTime is "HH:MM"; timezone is an IANA name.
func New(digestTime, timezone string, job Job, opts ...Option) (*Scheduler, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("scheduler: load timezone %q: %w", timezone, err)
	}

	parts := strings.Split(digestTime, ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("scheduler: invalid digest time %q: want HH:MM", digestTime)
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return nil, fmt.Errorf("scheduler: invalid hour in %q", digestTime)
	}
	min, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || min < 0 || min > 59 {
		return nil, fmt.Errorf("scheduler: invalid minute in %q", digestTime)
	}

	s := &Scheduler{
		hour: hour,
		min:  min,
		loc:  loc,
		job:  job,
		now:  time.Now,
		log:  slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// nextSlot computes the next daily slot strictly after now.
// If today's slot has not passed yet it returns it for today; otherwise the
// same hour:min on the next day.
func nextSlot(now time.Time, loc *time.Location, hour, min int) time.Time {
	y, m, d := now.In(loc).Date()
	candidate := time.Date(y, m, d, hour, min, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// todaySlot returns today's hour:min slot in loc relative to now.
func (s *Scheduler) todaySlot() time.Time {
	y, m, d := s.now().In(s.loc).Date()
	return time.Date(y, m, d, s.hour, s.min, 0, 0, s.loc)
}

// Run performs a start-up catch-up then loops firing the job at each slot until
// ctx is cancelled (graceful shutdown).
func (s *Scheduler) Run(ctx context.Context) error {
	// Catch-up: if today's slot already passed, try to run it (runJob guards
	// against a double run via the persisted last-run date).
	today := s.todaySlot()
	if !s.now().Before(today) {
		s.runJob(ctx, today)
	}

	for {
		now := s.now()
		next := nextSlot(now, s.loc, s.hour, s.min)
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-timer.C:
			s.runJob(ctx, next)
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// runJob runs the job for slot, honoring slot idempotency and logging errors
// without aborting the loop.
func (s *Scheduler) runJob(ctx context.Context, slot time.Time) {
	date := slot.Format(slotDateLayout)

	if s.getSlot != nil {
		last, err := s.getSlot(ctx)
		if err != nil {
			s.log.Error("read last digest date failed", "err", err)
		} else if last == date {
			s.log.Info("slot already run today, skipping", "date", date)
			return
		}
	}

	if err := s.job(ctx); err != nil {
		s.log.Error("digest job failed", "err", err, "slot", date)
		return
	}

	if s.setSlot != nil {
		if err := s.setSlot(ctx, date); err != nil {
			s.log.Error("persist last digest date failed", "err", err)
		}
	}
}
