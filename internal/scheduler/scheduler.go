// Package scheduler runs the digest pipeline on a schedule.
//
// MVP uses stdlib time (compute the next daily slot in a given timezone, wait
// via time.Timer with context cancellation). Slot idempotency across restarts
// is tracked in SQLite (meta table). See docs/TECHNICAL_PLAN.md §6 and
// SKILL.md §5. robfig/cron is added only when cron syntax is actually needed.
//
// Stage 0: interface + stub — real scheduling lands in Этап 4.
package scheduler

import "context"

// Job is the unit of work run on each tick (typically digest.Pipeline.Run).
type Job func(ctx context.Context) error

// Scheduler triggers a Job at a daily slot. Stage 0: stub.
type Scheduler struct {
	digestTime string // "HH:MM"
	timezone   string // IANA TZ
	job        Job
}

// New builds a Scheduler. Stage 0: stub.
func New(digestTime, timezone string, job Job) *Scheduler {
	return &Scheduler{digestTime: digestTime, timezone: timezone, job: job}
}

// Run blocks, firing the job at each daily slot until ctx is cancelled.
// Stage 0: stub (waits for cancellation without firing).
func (s *Scheduler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
