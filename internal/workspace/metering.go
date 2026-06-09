package workspace

import (
	"context"
	"io"
	"time"
)

// MeterFunc is called for every successful Put with the number of bytes
// written. Kept as a plain function value (not an interface) so the
// workspace package stays free of a dep on internal/usage — the gateway
// wires the two together at startup.
type MeterFunc func(ctx context.Context, agentID string, bytes int64)

// Metered wraps an existing Store to count bytes flowing through Put.
// Get / Stat / List / Delete / SignedURL pass through untouched.
type Metered struct {
	inner Store
	meter MeterFunc
}

// NewMetered returns a Store that tallies write volume per agent. meter
// must be non-nil; use the underlying Store directly if you don't want
// metering.
func NewMetered(inner Store, meter MeterFunc) *Metered {
	return &Metered{inner: inner, meter: meter}
}

// countingReader forwards bytes through and tallies them. Needed because
// Put accepts an io.Reader (not a []byte) — we can't know the payload
// size without either trusting the caller's `size` hint or counting as we
// read.
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
	// Prefer the caller's size when it's reliable; otherwise fall back
	// to the byte count we observed. Size=-1 means "don't know", which
	// is when the counting fallback matters most.
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

// LocalScopeDir forwards to the inner store when it implements
// LocalScoper (LocalFS does, S3 doesn't). Lets the workspace-reveal
// handler ask the public Store interface for an on-disk path
// without unwrapping Metered manually.
func (m *Metered) LocalScopeDir(agentID, projectID, sessionID string) (string, bool) {
	if ls, ok := m.inner.(LocalScoper); ok {
		return ls.LocalScopeDir(agentID, projectID, sessionID)
	}
	return "", false
}

// LocalScoper is implemented by stores whose objects live on the
// local filesystem (LocalFS today). A store that returns ok=true
// commits to: "this path is on the same disk as the daemon and
// safe to hand to `open`/`xdg-open`/`explorer`". Cloud stores
// (S3, R2) return ok=false — there's no host-side path to reveal.
type LocalScoper interface {
	LocalScopeDir(agentID, projectID, sessionID string) (string, bool)
}

// Compile-time check.
var (
	_ Store       = (*Metered)(nil)
	_ LocalScoper = (*Metered)(nil)
)
