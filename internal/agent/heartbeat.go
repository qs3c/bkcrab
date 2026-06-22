package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/bus"
)

const (
	// DefaultHeartbeatInterval 是心跳检查的默认间隔。
	DefaultHeartbeatInterval = 30 * time.Minute
)

// HeartbeatConfig 持有心跳配置。
type HeartbeatConfig struct {
	Interval time.Duration
}

// Heartbeat 运行定期检查并触发代理操作。
type Heartbeat struct {
	agent    *Agent
	bus      *bus.MessageBus
	interval time.Duration
}

// NewHeartbeat 为给定代理创建一个新的心跳。
func NewHeartbeat(ag *Agent, mb *bus.MessageBus, interval time.Duration) *Heartbeat {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	return &Heartbeat{
		agent:    ag,
		bus:      mb,
		interval: interval,
	}
}

// Start 开始心跳 goroutine。它会阻塞直到 ctx 被取消。
func (hb *Heartbeat) Start(ctx context.Context) {
	slog.Info("heartbeat started",
		"agent", hb.agent.Name(),
		"interval", hb.interval,
	)

	ticker := time.NewTicker(hb.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("heartbeat stopped", "agent", hb.agent.Name())
			return
		case <-ticker.C:
			hb.tick(ctx)
		}
	}
}

func (hb *Heartbeat) tick(ctx context.Context) {
	slog.Info("heartbeat tick", "agent", hb.agent.Name())

	// 1. 检查 HEARTBEAT.md 中的任务
	tasks := hb.loadHeartbeatTasks()
	if tasks != "" {
		// 代理默认时区（chatterUID="" → 代理/系统偏好，否则为服务器本地）：
		// 心跳没有聊天者，但 HEARTBEAT.md 的条件是用运维人员的挂钟时间编写的，
		// 而不是 Pod 的时间（托管部署上为 UTC）。
		now := time.Now().In(hb.agent.chatterLocation(""))
		heartbeatMsg := fmt.Sprintf(
			"[Heartbeat — %s]\nCurrent tasks from HEARTBEAT.md:\n%s\n\nReview these tasks and take action on any that need attention based on the current date/time.",
			now.Format("2006-01-02 15:04:05 -0700"),
			tasks,
		)

		// 通过消息总线作为入站消息发送
		hb.bus.Inbound <- bus.InboundMessage{
			Channel:  "heartbeat",
			ChatID:   "heartbeat_" + hb.agent.Name(),
			UserID:   "system",
			Text:     heartbeatMsg,
			PeerKind: "dm",
			Source:   bus.SourceHeartbeat,
		}
	}
}

func (hb *Heartbeat) loadHeartbeatTasks() string {
	path := filepath.Join(hb.agent.home(), "HEARTBEAT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return ""
	}
	return content
}
