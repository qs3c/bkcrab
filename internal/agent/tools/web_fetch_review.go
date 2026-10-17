package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// The review service is an evaluation-only loopback endpoint. Credentials stay
// in the platform relay; rejected page text is never returned to the model.
func reviewWebFetch(ctx context.Context, stage, target, content string) error {
	endpoint := os.Getenv("EVAL_FETCH_REVIEW_URL")
	if endpoint == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]string{"stage": stage, "url": target, "content": content})
	if err != nil {
		return fmt.Errorf("EVAL_FETCH_REVIEW_UNAVAILABLE")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("EVAL_FETCH_REVIEW_UNAVAILABLE")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("EVAL_FETCH_REVIEW_UNAVAILABLE：网页审查未完成，内容未放行；这不是违规判定")
	}
	defer response.Body.Close()
	if response.StatusCode == 403 {
		return fmt.Errorf("EVAL_FETCH_DENIED：网页涉及当前题目的现成答案，未返回内容；请查阅通用文档或继续本地分析")
	}
	var verdict struct {
		Allowed bool `json:"allowed"`
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	if response.StatusCode != 200 || readErr != nil || len(raw) > 65536 || json.Unmarshal(raw, &verdict) != nil || !verdict.Allowed {
		return fmt.Errorf("EVAL_FETCH_REVIEW_UNAVAILABLE：网页审查未完成，内容未放行；这不是违规判定")
	}
	return nil
}
