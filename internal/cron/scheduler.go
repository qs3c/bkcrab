package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/bus"
)

// JobType defines the type of cron schedule.
type JobType string

const (
	JobTypeExact    JobType = "exact"    // specific time: "14:30"
	JobTypeInterval JobType = "interval" // duration: "5m", "1h"
	JobTypeCron     JobType = "cron"     // cron expression: "*/5 * * * *"
)

// Job defines a scheduled job.
type Job struct {
	Name        string  `json:"name"`
	Type        JobType `json:"type"`
	Schedule    string  `json:"schedule"` // depends on type
	AgentID     string  `json:"agentId"`
	Channel     string  `json:"channel"`               // channel to send results back through
	ChatID      string  `json:"chatId"`                // chat to send results to
	Message     string  `json:"message"`               // message to send to the agent
	OwnerUserID string  `json:"ownerUserId,omitempty"` // bkclaw user that owns this job
}

// CronConfig holds cron job configuration.
type CronConfig struct {
	Jobs []Job `json:"jobs"`
}

// StoreInterface is the subset of store.Store needed by the scheduler.
type StoreInterface interface {
	GetDueCronJobs(ctx context.Context, now time.Time) ([]StoreJob, error)
	LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error)
	UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error
	// IncrementCronJobFailure bumps the row's consecutive-failure
	// counter and returns the new total. The scheduler self-deletes
	// the row once the counter crosses cronMaxConsecutiveFailures.
	IncrementCronJobFailure(ctx context.Context, jobID string) (int, error)
	DeleteCronJob(ctx context.Context, jobID string) error
	GetNextDueTime(ctx context.Context) (time.Time, error)
}

// ChannelChecker is the bit of channels.Manager the scheduler needs to
// pre-flight a tick: "is the bot adapter for (channel, accountID)
// actually registered right now?". Optional — without it the scheduler
// fires every due job blindly (legacy behaviour).
type ChannelChecker interface {
	Has(channel, accountID string) bool
}

// cronMaxConsecutiveFailures is the threshold at which a cron row gets
// auto-deleted because its destination channel has been missing for
// too many consecutive ticks. Tuned for the IM-bot use case: ~3
// minutes of dead destination is enough signal to give up (the bot
// adapter is gone, the user has to reschedule anyway).
const cronMaxConsecutiveFailures = 3

// StoreJob mirrors store.CronJobRecord to avoid import cycle. The fired
// job carries OwnerUserID resolved by the adapter (= agents.owner_user_id
// for the row's agent_id) so processInbound can route into the right
// user space.
type StoreJob struct {
	ID          string
	AgentID     string
	OwnerUserID string
	Name        string
	Type        string
	Schedule    string
	Message     string
	Channel     string
	ChatID      string
	AccountID   string
	// Timezone is the IANA zone the schedule is interpreted in —
	// captured from the chatter at creation time. Legacy rows carry
	// "UTC" (the old hardcoded value); empty means server-local.
	Timezone string
}

// globalNotify wakes the DB-mode scheduler when a cron tool creates or
// deletes a job, so it can recalculate its next sleep target immediately.
var globalNotify = make(chan struct{}, 1)

// NotifyJobCreated wakes the DB-mode scheduler. Non-blocking, safe from
// any goroutine.
func NotifyJobCreated() {
	select {
	case globalNotify <- struct{}{}:
	default:
	}
}

// Scheduler manages cron job execution.
type Scheduler struct {
	mu         sync.Mutex
	jobs       []Job
	bus        *bus.MessageBus
	store      StoreInterface
	channels   ChannelChecker
	instanceID string
	// hot-reload support
	parentCtx context.Context
	jobCancel context.CancelFunc
}

// SetChannelChecker enables pre-flight delivery checks. When set, jobs
// whose destination channel adapter is missing get their failure_count
// bumped instead of firing into the void; rows that hit
// cronMaxConsecutiveFailures consecutive misses are deleted.
func (s *Scheduler) SetChannelChecker(c ChannelChecker) {
	s.channels = c
}

// NewScheduler creates a scheduler from config.
func NewScheduler(jobs []Job, mb *bus.MessageBus) *Scheduler {
	return &Scheduler{
		jobs:       jobs,
		bus:        mb,
		instanceID: "default",
	}
}

// NewSchedulerFromStore returns a scheduler that polls the DB for due
// jobs on every tick — no in-memory job list. Each fired job carries its
// owning user_id (StoreJob.UserID) so processInbound can route into the
// right user space.
func NewSchedulerFromStore(st StoreInterface, mb *bus.MessageBus) *Scheduler {
	return &Scheduler{
		jobs:       nil,
		bus:        mb,
		store:      st,
		instanceID: "default",
	}
}

// SetStore enables DB-backed cron job polling.
func (s *Scheduler) SetStore(st StoreInterface) {
	s.store = st
}

// LoadJobs reads cron jobs from a JSON file.
func LoadJobs(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cron config: %w", err)
	}

	var cfg CronConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse cron config: %w", err)
	}

	return cfg.Jobs, nil
}

// Start begins the scheduler. It blocks until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.parentCtx = ctx
	slog.Info("cron scheduler started", "jobs", len(s.jobs), "store_backed", s.store != nil)

	// Start goroutines for initial in-memory jobs
	s.startJobGoroutines()
	s.mu.Unlock()

	// If store is set, poll for DB-backed jobs
	if s.store != nil {
		go s.pollStore(ctx)
	}

	<-ctx.Done()
	slog.Info("cron scheduler stopped")
}

func (s *Scheduler) pollStore(ctx context.Context) {
	for {
		s.processDueJobs(ctx)

		// Sleep precisely until the next job is due
		nextDue, err := s.store.GetNextDueTime(ctx)
		var sleepDur time.Duration
		if err != nil || nextDue.IsZero() {
			sleepDur = 5 * time.Minute // idle — no pending jobs
		} else {
			sleepDur = time.Until(nextDue)
			if sleepDur <= 0 {
				continue
			}
		}

		timer := time.NewTimer(sleepDur)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-globalNotify:
			timer.Stop() // new job created — recalculate
		case <-timer.C:
		}
	}
}

func (s *Scheduler) processDueJobs(ctx context.Context) {
	now := time.Now()
	dueJobs, err := s.store.GetDueCronJobs(ctx, now)
	if err != nil {
		slog.Error("failed to get due cron jobs", "error", err)
		return
	}

	for _, j := range dueJobs {
		locked, err := s.store.LockCronJob(ctx, j.ID, s.instanceID)
		if err != nil {
			slog.Error("failed to lock cron job", "id", j.ID, "error", err)
			continue
		}
		if !locked {
			continue
		}

		// Pre-flight: if the destination IM channel adapter isn't
		// registered (e.g. the bot token died and the gateway tore
		// it down), there's no point queuing the inbound — the
		// agent's reply would just hit "unknown outbound channel"
		// and be dropped. Instead, bump the failure counter; after
		// cronMaxConsecutiveFailures consecutive misses, delete
		// the row so the scheduler stops re-trying forever.
		// "web" is the dashboard SSE, "api" is the HTTP completions
		// endpoint — both are always reachable (replies go through
		// the plugin's channel.send, not an IM adapter). Empty
		// channel is a legacy row that doesn't route through any bot.
		if s.channels != nil && j.Channel != "" && j.Channel != "web" && j.Channel != "api" {
			if !s.channels.Has(j.Channel, j.AccountID) {
				count, ferr := s.store.IncrementCronJobFailure(ctx, j.ID)
				if ferr != nil {
					slog.Error("failed to bump cron failure count", "id", j.ID, "error", ferr)
					continue
				}
				if count >= cronMaxConsecutiveFailures {
					slog.Warn("auto-deleting cron job — destination channel missing for too many consecutive ticks",
						"id", j.ID, "name", j.Name,
						"channel", j.Channel, "account", j.AccountID,
						"failures", count)
					if derr := s.store.DeleteCronJob(ctx, j.ID); derr != nil {
						slog.Error("failed to delete dead cron job", "id", j.ID, "error", derr)
					}
					continue
				}
				slog.Warn("cron destination channel missing, skipping fire",
					"id", j.ID, "name", j.Name,
					"channel", j.Channel, "account", j.AccountID,
					"failures", count, "threshold", cronMaxConsecutiveFailures)
				continue
			}
		}

		slog.Info("firing store-backed cron job", "id", j.ID, "name", j.Name)

		text := j.Message
		if text == "" {
			text = fmt.Sprintf("[Cron Job: %s] This is a scheduled task trigger.", j.Name)
		}

		s.bus.Inbound <- bus.InboundMessage{
			Channel:     j.Channel,
			ChatID:      j.ChatID,
			UserID:      "cron",
			OwnerUserID: j.OwnerUserID,
			AgentID:     j.AgentID,
			Text:        text,
			PeerKind:    "dm",
			Source:      bus.SourceCron,
		}

		// Calculate next run based on job type.
		// UpdateCronJobRun also resets failure_count to 0 — a single
		// successful fire clears prior misses.
		switch j.Type {
		case "once":
			if err := s.store.DeleteCronJob(ctx, j.ID); err != nil {
				slog.Error("failed to delete once cron job", "id", j.ID, "error", err)
				farFuture := now.Add(100 * 365 * 24 * time.Hour)
				_ = s.store.UpdateCronJobRun(ctx, j.ID, now, farFuture)
			} else {
				slog.Info("once cron job completed and deleted", "id", j.ID, "name", j.Name)
			}
		case "interval":
			sched := strings.TrimPrefix(j.Schedule, "every ")
			dur, err := time.ParseDuration(sched)
			if err != nil {
				slog.Error("invalid interval schedule, disabling job", "id", j.ID, "schedule", j.Schedule)
				farFuture := now.Add(100 * 365 * 24 * time.Hour)
				_ = s.store.UpdateCronJobRun(ctx, j.ID, now, farFuture)
			} else {
				_ = s.store.UpdateCronJobRun(ctx, j.ID, now, now.Add(dur))
			}
		case "cron":
			_ = s.store.UpdateCronJobRun(ctx, j.ID, now, NextOccurrenceIn(j.Schedule, now, LocationOf(j.Timezone)))
		default:
			_ = s.store.UpdateCronJobRun(ctx, j.ID, now, now.Add(time.Hour))
		}
	}
}

func (s *Scheduler) runJob(ctx context.Context, job Job) {
	slog.Info("cron job registered", "name", job.Name, "type", job.Type, "schedule", job.Schedule)

	switch job.Type {
	case JobTypeInterval:
		s.runInterval(ctx, job)
	case JobTypeExact:
		s.runExact(ctx, job)
	case JobTypeCron:
		s.runCronExpr(ctx, job)
	default:
		slog.Warn("unknown cron job type", "name", job.Name, "type", job.Type)
	}
}

func (s *Scheduler) runInterval(ctx context.Context, job Job) {
	dur, err := time.ParseDuration(job.Schedule)
	if err != nil {
		slog.Error("invalid interval duration", "name", job.Name, "schedule", job.Schedule, "error", err)
		return
	}

	ticker := time.NewTicker(dur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.fireJob(job)
		}
	}
}

func (s *Scheduler) runExact(ctx context.Context, job Job) {
	// Parse time in HH:MM format
	parts := strings.Split(job.Schedule, ":")
	if len(parts) != 2 {
		slog.Error("invalid exact time format (expected HH:MM)", "name", job.Name, "schedule", job.Schedule)
		return
	}

	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		slog.Error("invalid exact time values", "name", job.Name, "schedule", job.Schedule)
		return
	}

	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}

		waitDur := time.Until(next)
		slog.Info("cron exact job scheduled", "name", job.Name, "next_fire", next.Format("2006-01-02 15:04:05"))

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitDur):
			s.fireJob(job)
		}
	}
}

func (s *Scheduler) runCronExpr(ctx context.Context, job Job) {
	// Simple cron expression parser: "minute hour day month weekday"
	// Supports * and */N for each field
	fields := strings.Fields(job.Schedule)
	if len(fields) != 5 {
		slog.Error("invalid cron expression (expected 5 fields)", "name", job.Name, "schedule", job.Schedule)
		return
	}

	// Check every minute
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if cronMatch(fields, t) {
				s.fireJob(job)
			}
		}
	}
}

// cronMatch checks if the current time matches a 5-field cron expression.
func cronMatch(fields []string, t time.Time) bool {
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, field := range fields {
		if !fieldMatch(field, values[i]) {
			return false
		}
	}
	return true
}

// fieldMatch checks if a value matches a cron field (* or */N or exact number).
func fieldMatch(field string, value int) bool {
	if field == "*" {
		return true
	}
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 {
			return false
		}
		return value%n == 0
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return n == value
}

func (s *Scheduler) fireJob(job Job) {
	slog.Info("cron job firing", "name", job.Name, "agent", job.AgentID, "owner", job.OwnerUserID)

	text := job.Message
	if text == "" {
		text = fmt.Sprintf("[Cron Job: %s] This is a scheduled task trigger.", job.Name)
	}

	s.bus.Inbound <- bus.InboundMessage{
		Channel:     job.Channel,
		ChatID:      job.ChatID,
		UserID:      "cron",
		OwnerUserID: job.OwnerUserID,
		AgentID:     job.AgentID,
		Text:        text,
		PeerKind:    "dm",
		Source:      bus.SourceCron,
	}
}

// UpdateJobs replaces the scheduler's job list (hot-reload).
// It cancels goroutines for old jobs and starts new ones.
func (s *Scheduler) UpdateJobs(jobs []Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.jobs = jobs

	// If the scheduler is running, restart job goroutines
	if s.parentCtx != nil {
		s.startJobGoroutines()
	}

	slog.Info("cron jobs updated (hot-reload)", "jobs", len(jobs))
}

// startJobGoroutines cancels any existing job goroutines and starts new ones.
// Must be called with s.mu held.
func (s *Scheduler) startJobGoroutines() {
	// Cancel previous batch of job goroutines
	if s.jobCancel != nil {
		s.jobCancel()
	}

	// Create a new child context for this batch
	jobCtx, cancel := context.WithCancel(s.parentCtx)
	s.jobCancel = cancel

	for _, job := range s.jobs {
		go s.runJob(jobCtx, job)
	}
}

// nextCronOccurrence finds the next time matching a 5-field cron
// expression after the given time, evaluated in server-local time.
// Kept for callers/tests that predate per-job timezones.
func nextCronOccurrence(schedule string, after time.Time) time.Time {
	return NextOccurrenceIn(schedule, after, time.Local)
}

// LocationOf resolves a job's stored timezone name to a *time.Location.
// Empty (pre-timezone rows / no chatter preference) and unknown names
// fall back to server-local — for legacy "UTC" rows that loads UTC,
// matching the behavior they were created under. Never returns nil.
func LocationOf(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		slog.Warn("cron: unknown timezone on job, using server-local", "timezone", name)
		return time.Local
	}
	return loc
}

// NextOccurrenceIn finds the next instant matching a 5-field cron
// expression after the given time, with the expression's wall-clock
// fields (minute/hour/day/month/weekday) read in loc — "0 9 * * *"
// stored with Asia/Shanghai fires at 09:00 Beijing time regardless of
// the server's TZ. Scans up to 48 hours ahead.
func NextOccurrenceIn(schedule string, after time.Time, loc *time.Location) time.Time {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return after.Add(time.Hour)
	}
	if loc == nil {
		loc = time.Local
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := after.Add(48 * time.Hour)
	for t.Before(limit) {
		if cronMatch(fields, t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return after.Add(time.Hour)
}
