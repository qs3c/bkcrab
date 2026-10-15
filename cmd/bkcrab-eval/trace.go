package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/qs3c/bkcrab/internal/provider"
	"sync/atomic"
	"time"
)

func redact(b []byte, secret string) []byte {
	if secret != "" {
		return bytes.ReplaceAll(b, []byte(secret), []byte("[REDACTED]"))
	}
	return b
}

type tracedProvider struct {
	inner provider.Provider
	sink  *evidence
	calls atomic.Int64
}

func (p *tracedProvider) request(m []provider.Message, t []provider.Tool, model string, n int, temp float64) int64 {
	id := p.calls.Add(1)
	p.sink.write("model_request", map[string]any{"call_id": id, "messages": m, "tools": t, "model": model, "max_tokens": n, "temperature": temp, "boundary": "provider interface before wire serialization"})
	return id
}
func (p *tracedProvider) failure(err error) {
	if err != nil {
		p.sink.mu.Lock()
		p.sink.failed = true
		p.sink.mu.Unlock()
	}
}
func (p *tracedProvider) Chat(c context.Context, m []provider.Message, t []provider.Tool, model string, n int, temp float64) (*provider.Response, error) {
	id := p.request(m, t, model, n, temp)
	start := time.Now()
	r, e := p.inner.Chat(c, m, t, model, n, temp)
	p.failure(e)
	p.sink.write("model_response", map[string]any{"call_id": id, "response": r, "error": errorText(e), "elapsed_seconds": time.Since(start).Seconds()})
	return r, e
}
func (p *tracedProvider) ChatStream(c context.Context, m []provider.Message, t []provider.Tool, model string, n int, temp float64) (*provider.StreamReader, error) {
	id := p.request(m, t, model, n, temp)
	r, e := p.inner.ChatStream(c, m, t, model, n, temp)
	if e != nil {
		p.failure(e)
		p.sink.write("model_error", map[string]any{"call_id": id, "error": e.Error()})
		return nil, e
	}
	ch := make(chan provider.StreamChunk)
	out := provider.NewStreamReader(ch)
	go func() {
		defer close(ch)
		for {
			chunk, ok := r.Next()
			if !ok {
				break
			}
			p.sink.write("model_stream_chunk", map[string]any{"call_id": id, "chunk": chunk})
			select {
			case ch <- chunk:
			case <-c.Done():
				out.SetErr(c.Err())
				return
			}
		}
		out.SetErr(r.Err())
		p.failure(r.Err())
		p.sink.write("model_stream_end", map[string]any{"call_id": id, "error": errorText(r.Err())})
	}()
	return out, nil
}
func errorText(e error) any {
	if e == nil {
		return nil
	}
	return fmt.Sprint(e)
}
