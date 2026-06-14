package workspace

import (
	"context"
	"io"
	"time"
)

// MeterFunc 在每次成功的 Put 时被调用，传入写入的字节数。
// 保持为普通函数值（而非接口），以便 workspace 包保持不依赖
// internal/usage——网关在启动时将两者连接在一起。
type MeterFunc func(ctx context.Context, agentID string, bytes int64)

// Metered 包装现有的 Store 以统计通过 Put 的字节数。
// Get / Stat / List / Delete / SignedURL 直接透传，不做处理。
type Metered struct {
	inner Store
	meter MeterFunc
}

// NewMetered 返回一个按代理统计写入量的 Store。meter 必须非 nil；
// 如果你不需要计量，请直接使用底层 Store。
func NewMetered(inner Store, meter MeterFunc) *Metered {
	return &Metered{inner: inner, meter: meter}
}

// countingReader 转发字节并通过时进行计数。之所以需要是因为
// Put 接受 io.Reader（而非 []byte）——如果不信任调用者的 `size` 提示
// 或在读取时计数，我们无法知道负载大小。
type countingReader struct {
	r       io.Reader
	n       int64
	doneErr error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF {
		c.doneErr = err
	}
	return n, err
}

func (m *Metered) Put(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string) error {
	cr := &countingReader{r: r}
	if err := m.inner.Put(ctx, agentID, projectID, sessionID, path, cr, size, contentType); err != nil {
		return err
	}
	// 当调用者提供的 size 可靠时优先使用；否则回退到我们观察到的
	// 字节计数。Size=-1 表示"未知"，这正是计数回退最关键的时候。
	n := size
	if n < 0 {
		n = cr.n
	}
	m.meter(ctx, agentID, n)
	return nil
}

func (m *Metered) Get(ctx context.Context, agentID, projectID, sessionID, path string) (io.ReadCloser, error) {
	return m.inner.Get(ctx, agentID, projectID, sessionID, path)
}

func (m *Metered) Stat(ctx context.Context, agentID, projectID, sessionID, path string) (*ObjectInfo, error) {
	return m.inner.Stat(ctx, agentID, projectID, sessionID, path)
}

func (m *Metered) List(ctx context.Context, agentID, projectID, sessionID string) ([]ObjectInfo, error) {
	return m.inner.List(ctx, agentID, projectID, sessionID)
}

func (m *Metered) Delete(ctx context.Context, agentID, projectID, sessionID, path string) error {
	return m.inner.Delete(ctx, agentID, projectID, sessionID, path)
}

func (m *Metered) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	return m.inner.Move(ctx, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID)
}

func (m *Metered) SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error) {
	return m.inner.SignedURL(ctx, agentID, projectID, sessionID, path, ttl)
}

// LocalScopeDir 当内部 store 实现了 LocalScoper 时转发给它
//（LocalFS 实现了，S3 没有）。允许工作区展示处理程序通过公开的
// Store 接口请求磁盘路径，而无需手动解包 Metered。
func (m *Metered) LocalScopeDir(agentID, projectID, sessionID string) (string, bool) {
	if ls, ok := m.inner.(LocalScoper); ok {
		return ls.LocalScopeDir(agentID, projectID, sessionID)
	}
	return "", false
}

// LocalScoper 由对象位于本地文件系统上的 store 实现（目前是 LocalFS）。
// 返回 ok=true 的 store 承诺："此路径与守护进程在同一磁盘上，
// 可安全传递给 `open`/`xdg-open`/`explorer`"。云存储（S3, R2）
// 返回 ok=false——没有主机端路径可以揭示。
type LocalScoper interface {
	LocalScopeDir(agentID, projectID, sessionID string) (string, bool)
}

// 编译时检查。
var (
	_ Store       = (*Metered)(nil)
	_ LocalScoper = (*Metered)(nil)
)
