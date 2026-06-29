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

	"github.com/qs3c/bkcrab/internal/bus"
)

// JobType 定义 cron 调度的类型。
type JobType string

const (
	JobTypeExact    JobType = "exact"    // 具体时间："14:30"
	JobTypeInterval JobType = "interval" // 持续时间："5m"、"1h"
	JobTypeCron     JobType = "cron"     // cron 表达式："*/5 * * * *"
)

// Job 定义一个定时作业。
type Job struct {
	Name        string  `json:"name"`
	Type        JobType `json:"type"`
	Schedule    string  `json:"schedule"` // 取决于类型
	AgentID     string  `json:"agentId"`
	Channel     string  `json:"channel"`               // 将结果发送回的渠道
	ChatID      string  `json:"chatId"`                // 要发送结果的聊天
	Message     string  `json:"message"`               // 发送给 agent 的消息
	OwnerUserID string  `json:"ownerUserId,omitempty"` // 拥有此作业的 bkcrab 用户
}

// CronConfig 保存 cron 作业配置。
type CronConfig struct {
	Jobs []Job `json:"jobs"`
}

// StoreInterface 是调度器所需的 store.Store 的子集。
type StoreInterface interface {
	GetDueCronJobs(ctx context.Context, now time.Time) ([]StoreJob, error)
	LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error)
	UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error
	// IncrementCronJobFailure 增加行的连续失败计数器并返回新总数。
	// 一旦计数器超过 cronMaxConsecutiveFailures，调度器将自动删除该行。
	IncrementCronJobFailure(ctx context.Context, jobID string) (int, error)
	DeleteCronJob(ctx context.Context, jobID string) error
	GetNextDueTime(ctx context.Context) (time.Time, error)
}

// ChannelChecker 是调度器在触发 tick 前预检所需的 channels.Manager 的一部分：
// "(channel, accountID) 的 bot 适配器当前是否实际已注册？" 可选的——
// 没有它时调度器会盲目触发所有到期的作业（旧版行为）。
type ChannelChecker interface {
	Has(channel, accountID string) bool
}

// cronMaxConsecutiveFailures 是 cron 行因目标渠道在连续多次 tick
// 中缺失而被自动删除的阈值。针对 IM-bot 用例调优：大约 3 分钟的目标
// 不可用就足以放弃（bot 适配器已消失，用户无论如何都需要重新调度）。
const cronMaxConsecutiveFailures = 3

// StoreJob 镜像 store.CronJobRecord 以避免导入循环。触发的作业携带
// 由适配器解析的 OwnerUserID（即该行 agent_id 对应的 agents.owner_user_id），
// 以便 processInbound 可以路由到正确的用户空间。
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
	// Timezone 是调度所依据的 IANA 时区——在创建时从对话者处捕获。
	// 旧版行携带 "UTC"（旧的硬编码值）；空表示服务器本地时间。
	Timezone string
}

// globalNotify 在 cron 工具创建或删除作业时唤醒 DB 模式调度器，
// 以便它能立即重新计算下一个休眠目标。
var globalNotify = make(chan struct{}, 1)

// NotifyJobCreated 唤醒 DB 模式调度器。非阻塞，可从任何 goroutine 安全调用。
func NotifyJobCreated() {
	select {
	case globalNotify <- struct{}{}:
	default:
	}
}

// Scheduler 管理 cron 作业的执行。
type Scheduler struct {
	mu         sync.Mutex
	jobs       []Job
	bus        *bus.MessageBus
	store      StoreInterface
	channels   ChannelChecker
	instanceID string
	// 热重载支持
	parentCtx context.Context
	jobCancel context.CancelFunc
}

// SetChannelChecker 启用预投递检查。设置后，目标渠道适配器缺失的作业
// 会递增 failure_count 而非空转；连续缺失达到 cronMaxConsecutiveFailures
// 次的行会被删除。
func (s *Scheduler) SetChannelChecker(c ChannelChecker) {
	s.channels = c
}

// NewScheduler 从配置创建调度器。
func NewScheduler(jobs []Job, mb *bus.MessageBus) *Scheduler {
	return &Scheduler{
		jobs:       jobs,
		bus:        mb,
		instanceID: "default",
	}
}

// NewSchedulerFromStore 返回一个调度器，在每个 tick 轮询数据库获取到期
// 作业——无需内存中的作业列表。每个触发的作业携带其所属用户的 user_id
// （StoreJob.UserID），以便 processInbound 能路由到正确的用户空间。
func NewSchedulerFromStore(st StoreInterface, mb *bus.MessageBus) *Scheduler {
	return &Scheduler{
		jobs:       nil,
		bus:        mb,
		store:      st,
		instanceID: "default",
	}
}

// SetStore 启用基于数据库的 cron 作业轮询。
func (s *Scheduler) SetStore(st StoreInterface) {
	s.store = st
}

// LoadJobs 从 JSON 文件读取 cron 作业。
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

// Start 启动调度器。它会阻塞直到 ctx 被取消。
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.parentCtx = ctx
	slog.Info("cron scheduler started", "jobs", len(s.jobs), "store_backed", s.store != nil)

	// 为初始的内存中作业启动 goroutine
	s.startJobGoroutines()
	s.mu.Unlock()

	// 如果设置了 store，轮询基于数据库的作业
	if s.store != nil {
		go s.pollStore(ctx)
	}

	<-ctx.Done()
	slog.Info("cron scheduler stopped")
}

func (s *Scheduler) pollStore(ctx context.Context) {
	for {
		s.processDueJobs(ctx)

		// 精确休眠直到下一个作业到期
		nextDue, err := s.store.GetNextDueTime(ctx)
		var sleepDur time.Duration
		if err != nil || nextDue.IsZero() {
			sleepDur = 5 * time.Minute // 空闲——无待处理的作业
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
			timer.Stop() // 新作业已创建——重新计算
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

		// 预检：如果目标 IM 渠道适配器未注册（例如 bot token 已失效且网关
		// 已拆除它），排队入站消息没有意义——agent 的回复只会遇到
		// "unknown outbound channel" 并被丢弃。相反，递增失败计数器；
		// 连续缺失 cronMaxConsecutiveFailures 次后，删除该行，让调度器
		// 停止无限重试。
		// "web" 是仪表板 SSE，"api" 是 HTTP completions 端点——
		// 两者始终可达（回复通过插件的 channel.send 而非 IM 适配器）。
		// 空渠道是不通过任何 bot 路由的旧版行。
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

		// 根据作业类型计算下一次运行时间。
		// UpdateCronJobRun 还会将 failure_count 重置为 0 ——
		// 一次成功触发即清除先前的缺失记录。
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
	// 解析 HH:MM 格式的时间
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
	// 简单 cron 表达式解析器："minute hour day month weekday"
	// 支持每个字段的 * 和 */N
	fields := strings.Fields(job.Schedule)
	if len(fields) != 5 {
		slog.Error("invalid cron expression (expected 5 fields)", "name", job.Name, "schedule", job.Schedule)
		return
	}

	// 每分钟检查一次
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

// cronMatch 检查当前时间是否匹配 5 字段 cron 表达式。
func cronMatch(fields []string, t time.Time) bool {
	values := []int{t.Minute(), t.Hour(), t.Day(), int(t.Month()), int(t.Weekday())}
	for i, field := range fields {
		if !fieldMatch(field, values[i]) {
			return false
		}
	}
	return true
}

// fieldMatch 检查值是否匹配 cron 字段（* 或 */N 或精确数字）。
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

// UpdateJobs 替换调度器的作业列表（热重载）。
// 它会取消旧作业的 goroutine 并启动新的。
func (s *Scheduler) UpdateJobs(jobs []Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.jobs = jobs

	// 如果调度器正在运行，重启作业 goroutine
	if s.parentCtx != nil {
		s.startJobGoroutines()
	}

	slog.Info("cron jobs updated (hot-reload)", "jobs", len(jobs))
}

// startJobGoroutines 取消所有现有作业 goroutine 并启动新的。
// 必须在持有 s.mu 时调用。
func (s *Scheduler) startJobGoroutines() {
	// 取消前一批作业 goroutine
	if s.jobCancel != nil {
		s.jobCancel()
	}

	// 为这一批创建新的子 context
	jobCtx, cancel := context.WithCancel(s.parentCtx)
	s.jobCancel = cancel

	for _, job := range s.jobs {
		go s.runJob(jobCtx, job)
	}
}

// nextCronOccurrence 查找在给定时间之后匹配 5 字段 cron 表达式的
// 下一个时间点，在服务器本地时间中计算。
// 保留给早于每作业时区的调用方/测试使用。
func nextCronOccurrence(schedule string, after time.Time) time.Time {
	return NextOccurrenceIn(schedule, after, time.Local)
}

// LocationOf 将作业存储的时区名称解析为 *time.Location。
// 空（无时区的旧版行/无对话者偏好）和未知名称回退到服务器本地时间——
// 对于旧版 "UTC" 行则加载 UTC，匹配它们被创建时的行为。永不返回 nil。
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

// NextOccurrenceIn 查找在给定时间之后匹配 5 字段 cron 表达式的下一个时刻，
// 表达式的挂钟字段（分/时/日/月/周几）在 loc 中读取——存储为
// Asia/Shanghai 的 "0 9 * * *" 无论服务器时区如何都在北京时间 09:00 触发。
// 最多扫描 48 小时。
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
