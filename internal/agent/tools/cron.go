package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/cron"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/store"
)

type createCronJobArgs struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Message  string `json:"message"`
	Type     string `json:"type"`
}

type deleteCronJobArgs struct {
	ID string `json:"id"`
}

// RegisterCronTools 注册 cron 作业管理工具。
//
// 从注册表中读取原始回合的频道 + chatID
// 在执行时通过 r.MessageChannel() / r.MessageChatID() 所以一个
// 代理构建中的注册处理每个聊天上下文
// 代理运行。代理循环的bindSession 标记每轮
// 在任何工具触发之前将值写入注册表。
func RegisterCronTools(r *Registry, st store.Store, userID, agentID string) {
	r.Register("create_cron_job",
		"Create a scheduled task. Use this for any user request that names a specific time, an interval, or a recurring schedule (e.g. \"5 分钟后提醒\", \"every Monday 9am\", \"each day at 8\"). When the schedule fires, the agent receives `message` as a fresh inbound prompt on the same channel the request originated from. Do NOT write timed reminders into HEARTBEAT.md — that file is only for conditional self-checks reviewed at every heartbeat tick.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Short task name (for listing / debugging).",
				},
				"schedule": map[string]interface{}{
					"type":        "string",
					"description": "When to fire, in the CHATTER'S local timezone (the timezone shown on the 'Current date/time' line of your system prompt) — write '每天早上 9 点' as '0 9 * * *' directly, do NOT convert to UTC. For type='cron': a 5-field cron expression like '0 9 * * *'. For type='interval': a duration like '5m' / '30m' / '2h'. For type='once': an ISO-8601 datetime like '2026-05-02T15:56:52' (no offset = chatter's local time; an explicit offset like '+08:00' or 'Z' is honored as written).",
				},
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The prompt the agent should receive when the schedule fires. Phrase it as instructions to yourself (e.g. \"提醒小m喝水\"), not as a user-facing message — the agent will compose the user reply when it processes the inbound.",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Schedule type. Use 'once' for one-shot reminders ('5 分钟后…'), 'cron' for calendar-style recurring schedules ('每天 9 点'), or 'interval' for fixed-period polling ('每 30 分钟检查一次'). Defaults to 'cron'.",
					"enum":        []string{"cron", "interval", "once"},
				},
			},
			"required": []string{"name", "schedule", "message"},
		},
		makeCreateCronJob(st, r, userID, agentID),
	)

	r.Register("list_cron_jobs",
		"List all scheduled tasks for this agent.",
		map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		makeListCronJobs(st, userID, agentID),
	)

	r.Register("delete_cron_job",
		"Delete a scheduled task by ID.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"id": map[string]interface{}{
					"type":        "string",
					"description": "The cron job ID to delete",
				},
			},
			"required": []string{"id"},
		},
		makeDeleteCronJob(st, userID),
	)
}

func makeCreateCronJob(st store.Store, r *Registry, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args createCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Name == "" || args.Schedule == "" || args.Message == "" {
			return "", fmt.Errorf("name, schedule, and message are required")
		}
		jobType := args.Type
		if jobType == "" {
			jobType = "cron"
		}

		// 在执行时读取原始总线地址——bindSession
		// 每回合都会标记它，因此这会捕获频道/聊天ID
		// 用户请求提醒时处于开机状态。
		channel := r.MessageChannel()
		chatID := r.MessageChatID()

		// 聊天者的有效时区决定了日程安排的方式
		// 阅读：无区域“一次”日期时间和 cron 挂钟字段
		// 两者都表示“他们的当地时间”（与系统相同的区域）
		// 提示的日期行呈现在）中，而不是服务器的日期行。这
		// 解析的名称被冻结到行上，以便调度程序保留
		// 即使喋喋不休后来发生变化，也要评估其中的重复情况。
		tzName := scope.Timezone(ctx, st, r.ChatterUserID(), agentID)
		loc := scope.LoadLocationOrLocal(tzName)

		id := generateUUID()
		now := time.Now()

		// 根据类型计算NextRun
		var nextRun time.Time
		switch jobType {
		case "once":
			t, err := time.Parse(time.RFC3339, args.Schedule)
			if err != nil {
				// 没有明确的偏移——在喋喋不休的区域进行解释。
				t, err = time.ParseInLocation("2006-01-02T15:04:05", args.Schedule, loc)
				if err != nil {
					return "", fmt.Errorf("once schedule must be ISO datetime (e.g. 2026-05-06T15:30:00), got: %q", args.Schedule)
				}
			}
			if t.Before(now) {
				return "", fmt.Errorf("schedule is in the past: %s", args.Schedule)
			}
			nextRun = t
		case "interval":
			sched := strings.TrimPrefix(args.Schedule, "every ")
			dur, err := time.ParseDuration(sched)
			if err != nil {
				return "", fmt.Errorf("invalid interval (e.g. '30m', '1h', 'every 2h'): %q", args.Schedule)
			}
			nextRun = now.Add(dur)
		default:
			// cron 表达式 — 在聊天区域中第一次出现。
			// （以前的 nextRun=now，它触发了一次作业
			// 创建后立即 - 一个虚假的提醒。）
			nextRun = cron.NextOccurrenceIn(args.Schedule, now, loc)
		}

		job := &store.CronJobRecord{
			ID:       id,
			AgentID:  agentID,
			Name:     args.Name,
			Type:     jobType,
			Schedule: args.Schedule,
			Message:  args.Message,
			Channel:  channel,
			ChatID:   chatID,
			// "" = 服务器本地；调度程序的 LocationOf 映射它
			// 与上面 LoadLocationOrLocal 的操作方式相同，因此创建
			// 和复发一致。
			Timezone:  tzName,
			Enabled:   true,
			NextRun:   &nextRun,
			CreatedAt: now,
		}

		if err := st.SaveCronJob(ctx, job); err != nil {
			return "", fmt.Errorf("save cron job: %w", err)
		}

		// 唤醒调度程序以接受这项新工作
		cron.NotifyJobCreated()

		// 回显有效时区+第一次触发，以便模型可以
		// confirm the local-time interpretation to the user ("好的，
		// 北京时间每天 9 点") instead of guessing.
		tzShown := tzName
		if tzShown == "" {
			tzShown = loc.String() + " (server default)"
		}
		return fmt.Sprintf("Cron job created successfully.\nID: %s\nName: %s\nSchedule: %s\nType: %s\nTimezone: %s\nFirst fire: %s",
			id, args.Name, args.Schedule, jobType, tzShown, nextRun.In(loc).Format("2006-01-02 15:04:05 -0700")), nil
	}
}

func makeListCronJobs(st store.Store, userID, agentID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		jobs, err := st.ListCronJobsByAgent(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("list cron jobs: %w", err)
		}
		filtered := jobs

		if len(filtered) == 0 {
			return "No cron jobs found for this agent.", nil
		}

		data, _ := json.MarshalIndent(filtered, "", "  ")
		return string(data), nil
	}
}

func makeDeleteCronJob(st store.Store, userID string) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args deleteCronJobArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		if err := st.DeleteCronJob(ctx, args.ID); err != nil {
			return "", fmt.Errorf("delete cron job: %w", err)
		}
		return fmt.Sprintf("Cron job %s deleted.", args.ID), nil
	}
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
