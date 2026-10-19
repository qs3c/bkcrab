package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// doLLMRequest retries only opening a response. A returned body is never replayed.
// All attempts share the caller's context and preserve the exact request body.
func doLLMRequest(client *http.Client, request *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(request)
		if request.Context().Err() != nil {
			return resp, err
		}
		retry := transientLLMError(err)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			retry = status == 429 || status == 502 || status == 503 || status == 504
		}
		if !retry || attempt >= 2 || request.GetBody == nil {
			return resp, err
		}
		delay := 500 * time.Millisecond * time.Duration(1<<attempt)
		if resp != nil {
			if value := resp.Header.Get("Retry-After"); value != "" {
				if seconds, e := strconv.Atoi(value); e == nil {
					if seconds > 10 {
						return resp, err
					}
					delay = max(delay, time.Duration(seconds)*time.Second)
				} else if date, e := http.ParseTime(value); e == nil {
					delay = max(delay, time.Until(date))
				}
			}
		}
		// Do not retry earlier than the server permits, or exceed a bounded backoff.
		if delay > 10*time.Second {
			return resp, err
		}
		if deadline, ok := request.Context().Deadline(); ok && time.Until(deadline) <= delay {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		slog.WarnContext(request.Context(), "retrying LLM request before response delivery", "attempt", attempt+1, "max_attempts", 3, "status", status, "delay_ms", delay.Milliseconds())
		timer := time.NewTimer(delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return nil, request.Context().Err()
		case <-timer.C:
		}
		body, bodyErr := request.GetBody()
		if bodyErr != nil {
			return nil, bodyErr
		}
		next := request.Clone(request.Context())
		next.Body = body
		request = next
	}
}

func transientLLMError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// net/http's response-header timeout also matches context.DeadlineExceeded.
	// doLLMRequest checks the caller's actual context before classifying errors,
	// so a transport timeout can retry without retrying an expired caller budget.
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTemporary || dns.IsTimeout
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}
