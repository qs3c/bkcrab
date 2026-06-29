package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
)

type setTimezoneArgs struct {
	Timezone string `json:"timezone"`
}

// RegisterTimezoneTool 注册 set_timezone — 结构化
// 对应于“将聊天者的时区写入 USER.md”。用户名.md
// 是模型可能会或可能不会采取行动的自由文本；这个工具坚持
// RUNTIME 读取它的时区（范围首选项），因此系统
// 提示符的日期行和 cron 调度切换到聊天者的本地
// 时间确定性而不是依赖于模型进行偏移
// 算术。
//
// 聊天在执行时通过 r.ChatterUserID() 解决 -
// BindSession 每回合都会对其进行标记 — 因此一次注册即可服务于每个回合
// 与代理交谈的发件人。
func RegisterTimezoneTool(r *Registry, st store.Store) {
	r.Register("set_timezone",
		"Record the current chatter's timezone. Call this whenever the chatter tells you their timezone, city, or country (e.g. \"我在北京\" → Asia/Shanghai), or when their messages imply one. The runtime uses it to show you their local time and to fire their scheduled tasks at the right local hour — do NOT just note the timezone in USER.md, that does not affect scheduling.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"timezone": map[string]interface{}{
					"type":        "string",
					"description": "IANA timezone name like 'Asia/Shanghai', 'Europe/Berlin', 'America/New_York'. Derive it from the city/country if the chatter didn't name a zone directly.",
				},
			},
			"required": []string{"timezone"},
		},
		makeSetTimezone(st, r),
	)
}

func makeSetTimezone(st store.Store, r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args setTimezoneArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Timezone == "" {
			return "", fmt.Errorf("timezone is required")
		}
		// “本地”在技术上是可加载的，但持久化是没有意义的——
		// 无论服务器的 TZ 发生什么，它都会将喋喋不休的信息固定下来
		// 处于读取时间。
		if args.Timezone == "Local" {
			return "", fmt.Errorf("timezone must be a concrete IANA name like 'Asia/Shanghai', not 'Local'")
		}
		loc, err := time.LoadLocation(args.Timezone)
		if err != nil {
			return "", fmt.Errorf("unknown timezone %q — use an IANA name like 'Asia/Shanghai': %w", args.Timezone, err)
		}
		chatterUID := r.ChatterUserID()
		if chatterUID == "" {
			return "", fmt.Errorf("no chatter identity on this turn — cannot persist timezone")
		}
		if err := scope.SaveUserTimezone(ctx, st, chatterUID, args.Timezone); err != nil {
			return "", fmt.Errorf("save timezone: %w", err)
		}
		return fmt.Sprintf("Timezone saved: %s. The chatter's local time is now %s. New scheduled tasks will fire in this timezone; existing ones keep the timezone they were created with.",
			args.Timezone, time.Now().In(loc).Format("2006-01-02 15:04:05 -0700 (Monday)")), nil
	}
}
