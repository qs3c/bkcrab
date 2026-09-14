package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/qs3c/bkcrab/internal/workspace"
	"io"
	"log/slog"
	"sync"
	"time"
)

type sandboxScope struct{ agentID, projectID, sessionID string }
type poolEntry struct {
	skillDirs []string
	scope     sandboxScope
	owner     string
	ex        Executor
	busy      bool
	lastUsed  time.Time
	cancel    context.CancelFunc
}

// Reservations cover creation, hydration, execution and persistence. Idle time
// starts on completion. Limits are per gateway, not distributed across replicas.
type LifecyclePool struct {
	inner                ExecutorPool
	workspace            workspace.Store
	idleTTL, sweep       time.Duration
	limits               Limits
	mu                   sync.Mutex
	entries              map[string]*poolEntry
	changed              chan struct{}
	waiting              int
	closed               bool
	startOnce, closeOnce sync.Once
	stopCh, done         chan struct{}
}

func NewLifecyclePool(inner ExecutorPool, idleTTL, sweep time.Duration) *LifecyclePool {
	if sweep <= 0 {
		sweep = 30 * time.Second
	}
	return &LifecyclePool{inner: inner, idleTTL: idleTTL, sweep: sweep,
		limits: Limits{MaxQueued: 64, QueueTimeout: 60 * time.Second}, entries: map[string]*poolEntry{},
		changed: make(chan struct{}), stopCh: make(chan struct{}), done: make(chan struct{})}
}

// SetLimits and SetWorkspace must be called before serving tools.
func (p *LifecyclePool) SetLimits(l Limits) { p.limits = l }

type workspaceAware interface{ SetWorkspace(workspace.Store) }

func (p *LifecyclePool) SetWorkspace(ws workspace.Store) {
	p.workspace = ws
	if sw, ok := p.inner.(workspaceAware); ok {
		sw.SetWorkspace(ws)
	}
}
func (p *LifecyclePool) Start() {
	p.startOnce.Do(func() {
		go func() {
			defer close(p.done)
			t := time.NewTicker(p.sweep)
			defer t.Stop()
			for {
				select {
				case <-p.stopCh:
					return
				case <-t.C:
					if p.idleTTL > 0 {
						p.evictIdle()
					}
				}
			}
		}()
	})
}
func (p *LifecyclePool) notifyLocked() { close(p.changed); p.changed = make(chan struct{}) }
func (p *LifecyclePool) availableLocked(owner string) bool {
	if p.limits.MaxContainers > 0 && len(p.entries) >= p.limits.MaxContainers {
		return false
	}
	if p.limits.MaxPerUser > 0 {
		n := 0
		for _, e := range p.entries {
			if e.owner == owner {
				n++
			}
		}
		if n >= p.limits.MaxPerUser {
			return false
		}
	}
	return true
}
func (p *LifecyclePool) acquire(ctx context.Context, sc sandboxScope, owner string) (*poolEntry, context.Context, error) {
	if owner == "" {
		owner = "system"
	}
	waitCtx := ctx
	if p.limits.QueueTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, p.limits.QueueTimeout)
		defer cancel()
	}
	key := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	queued := false
	defer func() {
		if queued {
			p.mu.Lock()
			p.waiting--
			p.mu.Unlock()
		}
	}()
	for {
		if err := waitCtx.Err(); err != nil {
			return nil, nil, fmt.Errorf("sandbox capacity wait: %w", err)
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, errors.New("sandbox pool is shutting down")
		}
		e := p.entries[key]
		if e == nil && !p.availableLocked(owner) {
			// Reclaim the oldest idle container under pressure instead of making
			// a request wait ten minutes for the normal idle sweep.
			owned := 0
			for _, entry := range p.entries {
				if entry.owner == owner {
					owned++
				}
			}
			userFull := p.limits.MaxPerUser > 0 && owned >= p.limits.MaxPerUser
			var victim *poolEntry
			for _, entry := range p.entries {
				if !entry.busy && (!userFull || entry.owner == owner) && (victim == nil || entry.lastUsed.Before(victim.lastUsed)) {
					victim = entry
				}
			}
			if victim != nil {
				victim.busy = true
				p.mu.Unlock()
				if err := p.remove(victim); err != nil {
					return nil, nil, fmt.Errorf("reclaim sandbox capacity: %w", err)
				}
				continue
			}
		}
		if e == nil && p.availableLocked(owner) {
			e = &poolEntry{scope: sc, owner: owner}
			p.entries[key] = e
		}
		if e != nil && !e.busy {
			e.busy = true
			opCtx, cancel := context.WithCancel(ctx)
			e.cancel = cancel
			p.mu.Unlock()
			if e.ex == nil || skillViewChanged(opCtx, e.skillDirs) {
				ex, err := p.inner.Get(WithUserID(opCtx, owner), sc.agentID, sc.projectID, sc.sessionID)
				if err != nil {
					cancel()
					p.mu.Lock()
					if e.ex == nil {
						delete(p.entries, key)
					} else {
						e.busy = false
						e.cancel = nil
					}
					p.notifyLocked()
					p.mu.Unlock()
					return nil, nil, err
				}
				e.ex = ex
				e.skillDirs = requestedSkillDirs(opCtx, e.skillDirs)
				if h, ok := ex.(WorkspaceHydrator); ok && p.workspace != nil {
					if err := h.HydrateWorkspace(opCtx, p.workspace, sc.agentID, sc.projectID, sc.sessionID); err != nil {
						removeErr := p.inner.Release(sc.agentID, sc.projectID, sc.sessionID)
						cancel()
						p.mu.Lock()
						if removeErr == nil {
							delete(p.entries, key)
						} else {
							e.busy = false
							e.ex = nil
							e.cancel = nil
						}
						p.notifyLocked()
						p.mu.Unlock()
						return nil, nil, fmt.Errorf("restore sandbox workspace: %w", err)
					}
				} else if _, selfHydrates := p.inner.(workspaceAware); !selfHydrates && p.workspace != nil {
					hydrateWorkspace(opCtx, p.workspace, ex, sc.agentID, sc.projectID, sc.sessionID, defaultSandboxRoot)
				}
			}
			return e, opCtx, nil
		}
		if !queued {
			if p.limits.MaxQueued > 0 && p.waiting >= p.limits.MaxQueued {
				p.mu.Unlock()
				return nil, nil, errors.New("sandbox queue is full; retry later")
			}
			p.waiting++
			queued = true
		}
		ch := p.changed
		p.mu.Unlock()
		select {
		case <-waitCtx.Done():
			return nil, nil, fmt.Errorf("sandbox capacity wait: %w", waitCtx.Err())
		case <-ch:
		}
	}
}
func (p *LifecyclePool) finish(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.busy = false
	e.lastUsed = time.Now()
	p.notifyLocked()
}
func (p *LifecyclePool) evictIdle() {
	p.mu.Lock()
	var evict []*poolEntry
	for _, e := range p.entries {
		if !e.busy && time.Since(e.lastUsed) > p.idleTTL {
			e.busy = true
			evict = append(evict, e)
		}
	}
	p.mu.Unlock()
	for _, e := range evict {
		if err := p.remove(e); err != nil {
			slog.Warn("sandbox eviction failed", "agent", e.scope.agentID, "error", err)
		}
	}
}
func (p *LifecyclePool) remove(e *poolEntry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e.ex != nil {
		if err := p.syncWorkspace(ctx, e.scope, e.ex); err != nil {
			p.finish(e)
			return err
		}
	}
	err := p.inner.Release(e.scope.agentID, e.scope.projectID, e.scope.sessionID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.entries, poolKey(e.scope.agentID, e.scope.projectID, e.scope.sessionID))
	} else {
		e.busy = false
		e.lastUsed = time.Now()
	}
	p.notifyLocked()
	return err
}
func (p *LifecyclePool) Release(agentID, projectID, sessionID string) error {
	p.mu.Lock()
	e := p.entries[poolKey(agentID, projectID, sessionID)]
	if e == nil {
		p.mu.Unlock()
		return nil
	}
	if e.busy {
		p.mu.Unlock()
		return errors.New("sandbox is busy; cancel its active operation before releasing it")
	}
	e.busy = true
	p.mu.Unlock()
	return p.remove(e)
}
func (p *LifecyclePool) CloseAll() {
	p.closeOnce.Do(func() {
		p.Start()
		p.mu.Lock()
		p.closed = true
		for _, e := range p.entries {
			if e.cancel != nil {
				e.cancel()
			}
		}
		p.notifyLocked()
		p.mu.Unlock()
		close(p.stopCh)
		<-p.done
		deadline := time.NewTimer(35 * time.Second)
		defer deadline.Stop()
		for {
			p.mu.Lock()
			busy := false
			for _, e := range p.entries {
				if e.busy {
					busy = true
					break
				}
			}
			ch := p.changed
			p.mu.Unlock()
			if !busy {
				break
			}
			select {
			case <-ch:
			case <-deadline.C:
				slog.Error("sandbox shutdown did not drain")
				return
			}
		}
		p.mu.Lock()
		entries := make([]*poolEntry, 0, len(p.entries))
		for _, e := range p.entries {
			e.busy = true
			entries = append(entries, e)
		}
		p.mu.Unlock()
		for _, e := range entries {
			if err := p.remove(e); err != nil {
				slog.Warn("sandbox shutdown sync failed; local data retained", "error", err)
			}
		}
		p.inner.CloseAll()
	})
}
func (p *LifecyclePool) syncWorkspace(ctx context.Context, sc sandboxScope, ex Executor) error {
	if p.workspace == nil {
		return nil
	}
	if s, ok := ex.(WorkspaceStreamer); ok {
		return s.SyncWorkspace(ctx, p.workspace, sc.agentID, sc.projectID, sc.sessionID)
	}
	s, ok := ex.(WorkspaceSnapshotter)
	if !ok {
		return nil
	}
	files, err := s.SnapshotWorkspace(ctx)
	if err != nil {
		return err
	}
	for path, data := range files {
		r, err := p.workspace.Get(ctx, sc.agentID, sc.projectID, sc.sessionID, path)
		if err == nil {
			old, readErr := io.ReadAll(io.LimitReader(r, int64(len(data))+1))
			r.Close()
			if readErr == nil && bytes.Equal(old, data) {
				continue
			}
		}
		if err := p.workspace.Put(ctx, sc.agentID, sc.projectID, sc.sessionID, path, bytes.NewReader(data), int64(len(data)), ""); err != nil {
			return err
		}
	}
	return nil
}
func (p *LifecyclePool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	dirs, pinned := ctx.Value(skillDirsKey{}).([]string)
	return &lazyExecutor{pool: p, skillDirs: dirs, pinnedSkills: pinned, scope: sandboxScope{agentID, projectID, sessionID}, owner: UserIDFromContext(ctx)}, nil
}
func (p *LifecyclePool) Backend() string { return p.inner.Backend() }

type lazyExecutor struct {
	skillDirs    []string
	pinnedSkills bool
	pool         *LifecyclePool
	scope        sandboxScope
	owner        string
}

func (l *lazyExecutor) run(ctx context.Context, write bool, fn func(context.Context, Executor) (string, error)) (string, error) {
	if l.pinnedSkills {
		ctx = WithSkillDirs(ctx, l.skillDirs)
	}
	owner := l.owner
	if owner == "" {
		owner = UserIDFromContext(ctx)
	}
	e, opCtx, err := l.pool.acquire(ctx, l.scope, owner)
	if err != nil {
		return "", err
	}
	defer l.pool.finish(e)
	out, opErr := fn(opCtx, e.ex)
	if write {
		// An interrupted command may still have produced valuable files.
		syncCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = l.pool.syncWorkspace(syncCtx, l.scope, e.ex)
		cancel()
		if err != nil {
			slog.Warn("sandbox workspace persistence failed", "agent", l.scope.agentID, "error", err)
			out += "\n[Workspace persistence failed; local files retained. Retry before moving to another host.]"
		}
	}
	return out, opErr
}
func (l *lazyExecutor) Exec(ctx context.Context, c string, t time.Duration) (string, error) {
	return l.run(ctx, true, func(ctx context.Context, e Executor) (string, error) { return e.Exec(ctx, c, t) })
}
func (l *lazyExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	return l.run(ctx, true, func(ctx context.Context, e Executor) (string, error) { return e.WriteFile(ctx, p, c) })
}
func (l *lazyExecutor) ReadFile(ctx context.Context, p string) (string, error) {
	return l.run(ctx, false, func(ctx context.Context, e Executor) (string, error) { return e.ReadFile(ctx, p) })
}
func (l *lazyExecutor) ListDir(ctx context.Context, p string) (string, error) {
	return l.run(ctx, false, func(ctx context.Context, e Executor) (string, error) { return e.ListDir(ctx, p) })
}
func (l *lazyExecutor) Close() error    { return nil }
func (l *lazyExecutor) Backend() string { return l.pool.Backend() }

var _ ExecutorPool = (*LifecyclePool)(nil)
