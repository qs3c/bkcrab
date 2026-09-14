package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// QuotaStore is a single-writer gateway guard for persistent object storage.
// Filesystem project quotas separately enforce writes made by shell processes.
// Deployments with multiple writers need a transactional distributed ledger.
type QuotaStore struct {
	Store
	maxBytes int64
	maxFiles int
	owner    func(context.Context, string) (string, error)
	agents   func(context.Context, string) ([]string, error)
	gate     chan struct{}
	usage    map[string]quotaUsage
}
type quotaUsage struct {
	bytes int64
	files int
}

func NewQuotaStore(inner Store, maxBytes int64, maxFiles int, owner func(context.Context, string) (string, error), agents func(context.Context, string) ([]string, error)) *QuotaStore {
	return &QuotaStore{Store: inner, maxBytes: maxBytes, maxFiles: maxFiles, owner: owner, agents: agents, gate: make(chan struct{}, 1), usage: map[string]quotaUsage{}}
}
func (q *QuotaStore) lock(ctx context.Context) error {
	select {
	case q.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (q *QuotaStore) unlock() { <-q.gate }
func (q *QuotaStore) used(ctx context.Context, user string) (quotaUsage, error) {
	if u, ok := q.usage[user]; ok {
		return u, nil
	}
	agents, err := q.agents(ctx, user)
	if err != nil {
		return quotaUsage{}, err
	}
	var u quotaUsage
	for _, agent := range agents {
		objects, err := q.Store.List(ctx, agent, "", "")
		if err != nil {
			return u, err
		}
		for _, o := range objects {
			if o.Size < 0 {
				return u, fmt.Errorf("cannot account for unknown object size")
			}
			u.bytes += o.Size
			u.files++
		}
	}
	q.usage[user] = u
	return u, nil
}
func (q *QuotaStore) Put(ctx context.Context, a, p, s, path string, r io.Reader, size int64, mime string) error {
	if size < 0 {
		return fmt.Errorf("workspace quota requires a known upload size")
	}
	if err := q.lock(ctx); err != nil {
		return err
	}
	defer q.unlock()
	user, err := q.owner(ctx, a)
	if err != nil {
		return err
	}
	u, err := q.used(ctx, user)
	if err != nil {
		return err
	}
	old, err := q.Store.Stat(ctx, a, p, s, path)
	oldSize := int64(0)
	newFile := 1
	if err == nil {
		oldSize = old.Size
		newFile = 0
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if oldSize < 0 {
		return fmt.Errorf("cannot account for previous object size")
	}
	if (q.maxBytes > 0 && size > q.maxBytes-(u.bytes-oldSize)) || (q.maxFiles > 0 && u.files+newFile > q.maxFiles) {
		return fmt.Errorf("workspace storage quota exceeded (limit %d bytes, %d files); delete files before uploading", q.maxBytes, q.maxFiles)
	}
	if err := q.Store.Put(ctx, a, p, s, path, io.LimitReader(r, size), size, mime); err != nil {
		delete(q.usage, user)
		return err
	}
	u.bytes += size - oldSize
	u.files += newFile
	q.usage[user] = u
	return nil
}
func (q *QuotaStore) Delete(ctx context.Context, a, p, s, path string) error {
	if err := q.lock(ctx); err != nil {
		return err
	}
	defer q.unlock()
	user, err := q.owner(ctx, a)
	if err != nil {
		return err
	}
	err = q.Store.Delete(ctx, a, p, s, path)
	delete(q.usage, user)
	return err
}
func (q *QuotaStore) Move(ctx context.Context, a, fp, fs, tp, ts string) error {
	if err := q.lock(ctx); err != nil {
		return err
	}
	defer q.unlock()
	user, err := q.owner(ctx, a)
	if err != nil {
		return err
	}
	err = q.Store.Move(ctx, a, fp, fs, tp, ts)
	delete(q.usage, user)
	return err
}
func (q *QuotaStore) LocalScopeDir(a, p, s string) (string, bool) {
	if local, ok := q.Store.(LocalScoper); ok {
		return local.LocalScopeDir(a, p, s)
	}
	return "", false
}
