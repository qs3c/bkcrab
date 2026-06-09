package taskqueue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// TaskStatus represents the current state of a task.
type TaskStatus string

const (
	TaskPending TaskStatus = "pending"
	TaskRunning TaskStatus = "running"
	TaskDone    TaskStatus = "done"
	TaskFailed  TaskStatus = "failed"
)

// Task represents a unit of work to be processed.
type Task struct {
	ID          string
	AgentID     string
	OwnerUserID string // owner of the agent (for user-space lookup)
	ChatKey     string // channel:chatID — serialization key
	Message     bus.InboundMessage
	AccountID   string
	Status      TaskStatus
	CreatedAt   time.Time
	StartedAt   *time.Time
	DoneAt      *time.Time
	Result      string
	Error       error
}

// TaskHandler processes a task and returns a result or error.
type TaskHandler func(ctx context.Context, task *Task) (string, error)

// chatQueue is a per-chat FIFO queue with its own processing goroutine.
type chatQueue struct {
	ch       chan *Task
	lastUsed time.Time
}

// Queue manages task submission, per-chat serialization, and global concurrency.
type Queue struct {
	maxConcurrent int
	taskTimeout   time.Duration
	idleTimeout   time.Duration

	mu        sync.Mutex
	tasks     map[string]*Task      // taskID -> Task
	chatQueues map[string]*chatQueue // chatKey -> chatQueue
	sem       chan struct{}          // counting semaphore for global concurrency
	handler   TaskHandler
	seq       uint64 // task ID sequence
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewQueue creates a new task queue.
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

	// Start idle cleanup goroutine
	go q.cleanupIdleQueues()

	return q
}

// Submit adds a task to the queue for processing.
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
		// Start a processing goroutine for this chat
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

// processChatQueue drains tasks for a single chat, running them serially.
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

// executeTask runs a single task with concurrency control and timeout.
func (q *Queue) executeTask(task *Task) {
	// Acquire global semaphore
	select {
	case q.sem <- struct{}{}:
	case <-q.ctx.Done():
		return
	}
	defer func() { <-q.sem }()

	// Mark running
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

	// Create timeout context
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

// cleanupIdleQueues removes chat queues that have been idle too long.
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

// RecentTasks returns recent tasks for observability, newest first.
func (q *Queue) RecentTasks(limit int) []*Task {
	q.mu.Lock()
	defer q.mu.Unlock()

	all := make([]*Task, 0, len(q.tasks))
	for _, t := range q.tasks {
		all = append(all, t)
	}

	// Sort newest first
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

	// Prune old completed tasks (keep last 200)
	if len(q.tasks) > 200 {
		go q.pruneOldTasks()
	}

	return all
}

// pruneOldTasks removes completed tasks beyond the retention limit.
func (q *Queue) pruneOldTasks() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.tasks) <= 200 {
		return
	}

	// Collect completed tasks sorted by creation time
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

	// Sort oldest first
	for i := 0; i < len(completed); i++ {
		for j := i + 1; j < len(completed); j++ {
			if completed[j].createdAt.Before(completed[i].createdAt) {
				completed[i], completed[j] = completed[j], completed[i]
			}
		}
	}

	// Remove oldest completed tasks to get below 200
	toRemove := len(q.tasks) - 200
	for i := 0; i < toRemove && i < len(completed); i++ {
		delete(q.tasks, completed[i].id)
	}
}

// Stop shuts down the queue.
func (q *Queue) Stop() {
	q.cancel()
}
