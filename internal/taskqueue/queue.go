package taskqueue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

// TaskStatus 表示任务的当前状态。
type TaskStatus string

const (
	TaskPending TaskStatus = "pending"
	TaskRunning TaskStatus = "running"
	TaskDone    TaskStatus = "done"
	TaskFailed  TaskStatus = "failed"
)

// Task 表示一个待处理的工作单元。
type Task struct {
	ID          string
	AgentID     string
	OwnerUserID string // 代理的所有者（用于用户空间查找）
	ChatKey     string // channel:chatID — 序列化键
	Message     bus.InboundMessage
	AccountID   string
	Status      TaskStatus
	CreatedAt   time.Time
	StartedAt   *time.Time
	DoneAt      *time.Time
	Result      string
	Error       error
}

// TaskHandler 处理任务并返回结果或错误。
type TaskHandler func(ctx context.Context, task *Task) (string, error)

// chatQueue 是每个聊天独立的 FIFO 队列，拥有自己的处理 goroutine。
type chatQueue struct {
	ch       chan *Task
	lastUsed time.Time
}

// Queue 管理任务提交、聊天级别串行化和全局并发控制。
type Queue struct {
	maxConcurrent int
	taskTimeout   time.Duration
	idleTimeout   time.Duration

	mu         sync.Mutex
	tasks      map[string]*Task      // taskID -> 任务
	chatQueues map[string]*chatQueue // chatKey -> 聊天队列
	sem        chan struct{}         // 全局并发的计数信号量
	handler    TaskHandler
	seq        uint64 // 任务 ID 序列号
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewQueue 创建一个新的任务队列。
func NewQueue(maxConcurrent int, taskTimeout time.Duration, handler TaskHandler) *Queue {
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	if taskTimeout <= 0 {
		taskTimeout = 5 * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())

	q := &Queue{
		maxConcurrent: maxConcurrent,
		taskTimeout:   taskTimeout,
		idleTimeout:   5 * time.Minute,
		tasks:         make(map[string]*Task),
		chatQueues:    make(map[string]*chatQueue),
		sem:           make(chan struct{}, maxConcurrent),
		handler:       handler,
		ctx:           ctx,
		cancel:        cancel,
	}

	// 启动空闲清理 goroutine
	go q.cleanupIdleQueues()

	return q
}

// Submit 将任务添加到队列中等待处理。
func (q *Queue) Submit(agentID, chatKey string, msg bus.InboundMessage, accountID string) string {
	q.mu.Lock()

	q.seq++
	taskID := fmt.Sprintf("task-%d-%d", time.Now().UnixMilli(), q.seq)

	task := &Task{
		ID:          taskID,
		AgentID:     agentID,
		OwnerUserID: msg.OwnerUserID,
		ChatKey:     chatKey,
		Message:     msg,
		AccountID:   accountID,
		Status:      TaskPending,
		CreatedAt:   time.Now(),
	}
	q.tasks[taskID] = task

	cq, ok := q.chatQueues[chatKey]
	if !ok {
		cq = &chatQueue{
			ch:       make(chan *Task, 100),
			lastUsed: time.Now(),
		}
		q.chatQueues[chatKey] = cq
		// 为该聊天启动处理 goroutine
		go q.processChatQueue(chatKey, cq)
	}
	cq.lastUsed = time.Now()

	pendingCount := len(cq.ch)
	q.mu.Unlock()

	slog.Info("task submitted",
		"task_id", taskID,
		"chat_key", chatKey,
		"agent_id", agentID,
		"queue_depth", pendingCount+1,
	)

	if pendingCount > 100 {
		slog.Warn("queue depth high", "chat_key", chatKey, "depth", pendingCount+1)
	}

	cq.ch <- task
	return taskID
}

// processChatQueue 从单个聊天的队列中取出任务，串行执行。
func (q *Queue) processChatQueue(chatKey string, cq *chatQueue) {
	for {
		select {
		case <-q.ctx.Done():
			return
		case task, ok := <-cq.ch:
			if !ok {
				return
			}
			q.executeTask(task)

			q.mu.Lock()
			cq.lastUsed = time.Now()
			q.mu.Unlock()
		}
	}
}

// executeTask 运行单个任务，带并发控制和超时。
func (q *Queue) executeTask(task *Task) {
	// 获取全局信号量
	select {
	case q.sem <- struct{}{}:
	case <-q.ctx.Done():
		return
	}
	defer func() { <-q.sem }()

	// 标记为运行中
	now := time.Now()
	q.mu.Lock()
	task.Status = TaskRunning
	task.StartedAt = &now
	concurrent := len(q.sem)
	q.mu.Unlock()

	slog.Info("task started",
		"task_id", task.ID,
		"agent_id", task.AgentID,
		"chat_key", task.ChatKey,
		"concurrent_count", concurrent,
	)

	// 创建超时上下文
	ctx, cancel := context.WithTimeout(q.ctx, q.taskTimeout)
	defer cancel()

	result, err := q.handler(ctx, task)

	doneAt := time.Now()
	duration := doneAt.Sub(*task.StartedAt)

	q.mu.Lock()
	task.DoneAt = &doneAt
	task.Result = result
	task.Error = err
	if err != nil {
		task.Status = TaskFailed
	} else {
		task.Status = TaskDone
	}
	q.mu.Unlock()

	if err != nil {
		slog.Error("task failed",
			"task_id", task.ID,
			"agent_id", task.AgentID,
			"chat_key", task.ChatKey,
			"duration_ms", duration.Milliseconds(),
			"error", err,
		)
	} else {
		slog.Info("task completed",
			"task_id", task.ID,
			"agent_id", task.AgentID,
			"chat_key", task.ChatKey,
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// cleanupIdleQueues 移除空闲时间过长的聊天队列。
func (q *Queue) cleanupIdleQueues() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-ticker.C:
			q.mu.Lock()
			now := time.Now()
			for key, cq := range q.chatQueues {
				if now.Sub(cq.lastUsed) > q.idleTimeout && len(cq.ch) == 0 {
					close(cq.ch)
					delete(q.chatQueues, key)
					slog.Debug("idle chat queue removed", "chat_key", key)
				}
			}
			q.mu.Unlock()
		}
	}
}

// RecentTasks 返回最近的任务以供可观测性查看，最新的在前。
func (q *Queue) RecentTasks(limit int) []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	all := make([]*Task, 0, len(q.tasks))
	for _, t := range q.tasks {
		all = append(all, t)
	}

	// 按最新时间排序
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].CreatedAt.After(all[i].CreatedAt) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}

	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}

	// 清理旧的已完成任务（保留最近 200 个）
	if len(q.tasks) > 200 {
		go q.pruneOldTasks()
	}

	return all
}

// pruneOldTasks 移除超出保留限制的已完成任务。
func (q *Queue) pruneOldTasks() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) <= 200 {
		return
	}

	// 收集已完成任务并按创建时间排序
	type entry struct {
		id        string
		createdAt time.Time
	}
	var completed []entry
	for id, t := range q.tasks {
		if t.Status == TaskDone || t.Status == TaskFailed {
			completed = append(completed, entry{id, t.CreatedAt})
		}
	}

	// 按最早时间排序
	for i := 0; i < len(completed); i++ {
		for j := i + 1; j < len(completed); j++ {
			if completed[j].createdAt.Before(completed[i].createdAt) {
				completed[i], completed[j] = completed[j], completed[i]
			}
		}
	}

	// 移除最早的已完成任务，使总数低于 200
	toRemove := len(q.tasks) - 200
	for i := 0; i < toRemove && i < len(completed); i++ {
		delete(q.tasks, completed[i].id)
	}
}

// Stop 关闭队列。
func (q *Queue) Stop() {
	q.cancel()
}
