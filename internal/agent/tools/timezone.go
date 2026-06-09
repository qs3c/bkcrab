package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/scope"
	"github.com/bkclaw-ai/bkclaw/internal/store"
)

type setTimezoneArgs struct {
	Timezone string `json:"timezone"`
}

// RegisterTimezoneTool registers set_timezone — the structured
// counterpart to "write the chatter's timezone into USER.md". USER.md
// is free text the model may or may not act on; this tool persists the
// timezone where the RUNTIME reads it (scope prefs), so the system
// prompt's date line and cron scheduling switch to the chatter's local
// time deterministically instead of relying on the model doing offset
// arithmetic.
//
// The chatter is resolved at execute time via r.ChatterUserID() —
// bindSession stamps it per-turn — so one registration serves every
// sender the agent talks to.
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
		// "Local" is technically loadable but meaningless to persist —
		// it would pin the chatter to whatever the server's TZ happens
		// to be at read time.
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
