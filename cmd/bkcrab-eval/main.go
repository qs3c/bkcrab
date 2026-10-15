// bkcrab-eval runs one task through the production Agent in an externally isolated environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var work, home, out, prompt, model, endpoint, keyEnv string
	var maxTokens, window, iterations int
	var timeout time.Duration
	flag.StringVar(&work, "workspace", "", "absolute task workspace")
	flag.StringVar(&home, "home", "", "fresh isolated state directory")
	flag.StringVar(&out, "output", "", "evidence directory")
	flag.StringVar(&prompt, "prompt-file", "", "task instruction file")
	flag.StringVar(&model, "model", "", "provider model id")
	flag.StringVar(&endpoint, "api-base", "", "OpenAI-compatible API base")
	flag.StringVar(&keyEnv, "api-key-env", "EVAL_MODEL_API_KEY", "credential environment variable")
	flag.IntVar(&maxTokens, "max-tokens", 8192, "maximum output tokens per call")
	flag.IntVar(&window, "context-window", 131072, "context capacity, recorded explicitly")
	flag.IntVar(&iterations, "max-iterations", 100, "tool iteration limit")
	flag.DurationVar(&timeout, "timeout", 20*time.Minute, "whole task wall-clock limit")
	flag.Parse()
	for _, p := range []string{work, home, out, prompt} {
		if !filepath.IsAbs(p) {
			return errors.New("workspace, home, output and prompt-file must be absolute")
		}
	}
	if model == "" || endpoint == "" || maxTokens <= 0 || window <= maxTokens || iterations <= 0 || timeout <= 0 {
		return errors.New("invalid model configuration or limits")
	}
	if fi, err := os.Stat(work); err != nil || !fi.IsDir() {
		return errors.New("workspace must exist")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		return errors.New("home must be new for each attempt")
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	if err := os.Setenv("BKCRAB_HOME", home); err != nil {
		return err
	}
	text, err := os.ReadFile(prompt)
	if err != nil {
		return err
	}
	key := os.Getenv(keyEnv)
	if key == "" {
		return fmt.Errorf("missing credential environment variable %s", keyEnv)
	}
	sink, err := newEvidence(filepath.Join(out, "events.jsonl"), key)
	if err != nil {
		return err
	}
	defer sink.file.Close()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	prov := &tracedProvider{inner: provider.NewOpenAI(key, endpoint), sink: sink}
	disabled := false
	rc := config.ResolvedAgent{ID: "eval", UserID: "eval", DisplayName: "BKCrab", Home: home, Workspace: work, Model: model, MaxTokens: maxTokens, ContextWindow: window, Temperature: 0, MaxToolIterations: iterations, MaxParallelToolCalls: 4, AutoPersist: &disabled}
	ws := &taskWorkspace{LocalFS: workspace.NewLocalFS(filepath.Dir(work)), name: filepath.Base(work)}
	manager, err := agent.NewManager([]config.ResolvedAgent{rc}, prov, bus.New(), agent.WithUserID("eval"), agent.WithWorkspaceStore(ws), agent.WithSkillsLearner(config.SkillsLearnerCfg{Enabled: &disabled}))
	if err != nil {
		return err
	}
	defer manager.CloseAll()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, timeout)
	defer deadline()
	ctx = agent.ContextWithStream(ctx, nil, sink, nil, "eval", "eval", "trial")
	sink.write("run_config", map[string]any{"model": model, "endpoint": endpoint, "max_tokens": maxTokens, "context_window": window, "max_iterations": iterations, "profile": "isolated-coding", "disabled_services": []string{"gateway", "IM", "RAG", "MCP", "cross-task memory", "skills learning", "database goals/cron"}, "workspace": work})
	started := time.Now()
	answer := manager.DefaultAgent().HandleMessage(ctx, bus.InboundMessage{Channel: "eval", ChatID: "trial", UserID: "eval", OwnerUserID: "eval", AgentID: "eval", Text: string(text)})
	reason := "completed"
	if ctx.Err() != nil {
		reason = "cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
	}
	sink.mu.Lock()
	modelFailed := sink.failed
	iterationCap := sink.iterationCap
	sink.mu.Unlock()
	if iterationCap && reason == "completed" {
		reason = "iteration_limit"
	}
	if modelFailed && reason == "completed" {
		reason = "agent_error"
	}
	sink.write("run_end", map[string]any{"reason": reason, "final_response": answer, "elapsed_seconds": time.Since(started).Seconds()})
	sink.mu.Lock()
	logErr := sink.err
	sink.mu.Unlock()
	if err := sink.file.Sync(); err != nil {
		return err
	}
	if logErr != nil {
		return logErr
	}
	if reason != "completed" {
		return fmt.Errorf("task stopped: %s", reason)
	}
	return nil
}

type evidence struct {
	mu           sync.Mutex
	file         *os.File
	secret       string
	seq          int64
	err          error
	failed       bool
	iterationCap bool
}

func newEvidence(path, secret string) (*evidence, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	return &evidence{file: f, secret: secret}, e
}
func (s *evidence) write(kind string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	b, e := json.Marshal(map[string]any{"seq": s.seq, "time": time.Now().UTC(), "type": kind, "data": data})
	if e == nil {
		b = redact(b, s.secret)
		_, e = s.file.Write(append(b, '\n'))
	}
	if e != nil {
		s.err = e
	}
}
func (s *evidence) AppendSessionEvent(_ context.Context, _, _, _, kind string, data []byte) (int64, error) {
	s.write(kind, json.RawMessage(data))
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "error" {
		s.failed = true
	}
	var payload struct {
		Metadata struct {
			IterationCapReached bool `json:"iterationCapReached"`
		} `json:"metadata"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Metadata.IterationCapReached {
		s.iterationCap = true
	}
	return s.seq, s.err
}

// Scope mapping is an evaluation transport concern. All real tools use the same task root.
type taskWorkspace struct {
	*workspace.LocalFS
	name string
}

func (w *taskWorkspace) LocalScopeDir(_, _, _ string) (string, bool) {
	return w.LocalFS.LocalScopeDir(w.name, "", "")
}
func (w *taskWorkspace) Put(c context.Context, _, _, _, p string, r io.Reader, n int64, t string) error {
	return w.LocalFS.Put(c, w.name, "", "", p, r, n, t)
}
func (w *taskWorkspace) Get(c context.Context, _, _, _, p string) (io.ReadCloser, error) {
	return w.LocalFS.Get(c, w.name, "", "", p)
}
func (w *taskWorkspace) Stat(c context.Context, _, _, _, p string) (*workspace.ObjectInfo, error) {
	return w.LocalFS.Stat(c, w.name, "", "", p)
}
func (w *taskWorkspace) List(c context.Context, _, _, _ string) ([]workspace.ObjectInfo, error) {
	return w.LocalFS.List(c, w.name, "", "")
}
func (w *taskWorkspace) Delete(c context.Context, _, _, _, p string) error {
	return w.LocalFS.Delete(c, w.name, "", "", p)
}
func (w *taskWorkspace) Move(context.Context, string, string, string, string, string) error {
	return errors.New("evaluation task workspace cannot move")
}
func (w *taskWorkspace) SignedURL(context.Context, string, string, string, string, time.Duration) (string, error) {
	return "", workspace.ErrSignedURLUnsupported
}
