package scope

import (
	"context"
	"errors"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

// PrefsNamespace 是 kind=setting 命名空间，用于存储每个对话者的
// 偏好设置（当前为时区；后续可扩展语言等）。数据行与其他设置一样
// 存储在 configs 中，但解析顺序与 scope.Setting 不同：对话者自身的偏好
// 必须覆盖智能体的默认值，因此遍历顺序为对话者优先（见下方 Timezone），
// 而非通常的外层→内层合并（智能体层会遮蔽用户层）。
const PrefsNamespace = "prefs"

// prefsTimezoneKey 是 prefs 命名空间内存储 IANA 时区名称（如 "Asia/Shanghai"）的 JSON 键。
const prefsTimezoneKey = "timezone"

// Timezone 解析对话者与智能体对话时的有效 IANA 时区名称。最具体者优先：
//
//	(chatter, agent) → (chatter, '') → ('', agent) → ('', '')
//
// 即优先使用 (对话者, 智能体) 级覆盖，然后是对话者的个人设置（跟随其跨智能体），
// 然后是智能体默认值，最后是系统默认值。未设置时返回 "" —— 调用方回退到服务器本地时间。
func Timezone(ctx context.Context, st store.Store, chatterUID, agentID string) string {
	if st == nil {
		return ""
	}
	type layer struct{ uid, aid string }
	layers := []layer{}
	if chatterUID != "" && agentID != "" {
		layers = append(layers, layer{chatterUID, agentID})
	}
	if chatterUID != "" {
		layers = append(layers, layer{chatterUID, ""})
	}
	if agentID != "" {
		layers = append(layers, layer{"", agentID})
	}
	layers = append(layers, layer{"", ""})
	for _, l := range layers {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, l.uid, l.aid, PrefsNamespace)
		if err != nil || rec == nil {
			continue
		}
		if tz, ok := rec.Data[prefsTimezoneKey].(string); ok && tz != "" {
			return tz
		}
	}
	return ""
}

// LoadLocationOrLocal 将 IANA 时区名称解析为 *time.Location，
// 当名称为空或未知时回退到服务器本地时间。永不返回 nil。
func LoadLocationOrLocal(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}

// SaveUserTimezone 将 tz 记录为 userID 在用户作用域（agent_id=""）下的个人时区，
// 使其跟随对话者跨所有智能体。用户 prefs 行中已有的其他键将被保留。
// tz 必须是有效的 IANA 名称 —— 调用前请使用 time.LoadLocation 验证。
func SaveUserTimezone(ctx context.Context, st store.Store, userID, tz string) error {
	if st == nil {
		return errors.New("scope.SaveUserTimezone: store is required")
	}
	if userID == "" {
		return errors.New("scope.SaveUserTimezone: userID is required")
	}
	data := map[string]interface{}{}
	if rec, err := st.GetConfigByName(ctx, store.KindSetting, userID, "", PrefsNamespace); err == nil && rec != nil {
		for k, v := range rec.Data {
			data[k] = v
		}
	}
	data[prefsTimezoneKey] = tz
	return SaveSetting(ctx, st, userID, "", PrefsNamespace, data)
}
