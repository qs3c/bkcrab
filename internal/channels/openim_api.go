package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
)

func NormalizeOpenIMConfig(c config.OpenIMConfig) (config.OpenIMConfig, error) {
	u, err := url.Parse(strings.TrimSpace(c.APIURL))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("openim: apiUrl must be an http(s) base URL without credentials, query or fragment")
	}
	u.Host = strings.ToLower(u.Host)
	c.APIURL = strings.TrimRight(u.String(), "/")
	c.WSURL = strings.TrimSpace(c.WSURL)
	if c.WSURL != "" {
		ws, err := url.Parse(c.WSURL)
		if err != nil || ws == nil || (ws.Scheme != "ws" && ws.Scheme != "wss") || ws.Hostname() == "" || ws.User != nil || ws.RawQuery != "" || ws.Fragment != "" {
			return c, errors.New("openim: wsUrl must be a ws(s) URL without credentials, query or fragment")
		}
		ws.Host = strings.ToLower(ws.Host)
		c.WSURL = ws.String()
	}
	c.AdminUserID = strings.TrimSpace(c.AdminUserID)
	c.BotUserID = strings.TrimSpace(c.BotUserID)
	if c.AdminUserID == "" || c.AdminSecret == "" || c.BotUserID == "" {
		return c, errors.New("openim: adminUserId, adminSecret and botUserId are required")
	}
	if c.AdminUserID == c.BotUserID {
		return c, errors.New("openim: use a separate ordinary user as the bot, not the administrator")
	}
	if len(c.BotUserID) > 64 || len(c.AdminUserID) > 256 || len(c.AllowedGroupIDs) > 1000 {
		return c, errors.New("openim: too many groups or user ID too long")
	}
	groups := make([]string, 0, len(c.AllowedGroupIDs))
	seen := make(map[string]bool)
	for _, group := range c.AllowedGroupIDs {
		group = strings.TrimSpace(group)
		if len(group) > 64 {
			return c, errors.New("openim: group ID too long")
		}
		if group != "" && !seen[group] {
			groups = append(groups, group)
			seen[group] = true
		}
	}
	c.AllowedGroupIDs = groups
	return c, nil
}

func openIMHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// IDs are stable across secret rotation. Use the same canonical URL for all bots
// of one instance; alternate DNS names cannot be inferred to be equivalent.
func OpenIMInstanceID(c config.OpenIMConfig) string { return openIMHash(c.APIURL)[:32] }
func OpenIMAccountID(c config.OpenIMConfig) string {
	return OpenIMInstanceID(c) + "-" + openIMHash(c.BotUserID)[:32]
}

// OpenIM's callback client cannot set an application Authorization header.
// The instance URL contains this scoped HMAC capability, never the admin secret.
func OpenIMWebhookSecret(c config.OpenIMConfig) string {
	m := hmac.New(sha256.New, []byte(c.AdminSecret))
	m.Write([]byte("bkcrab/openim/webhook/v1\x00" + c.APIURL + "\x00" + c.AdminUserID))
	return hex.EncodeToString(m.Sum(nil))
}

type openIMAPIError struct{ Code int }

func (e *openIMAPIError) Error() string { return fmt.Sprintf("openim: API error %d", e.Code) }

func (o *OpenIM) post(ctx context.Context, path, token string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.APIURL+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("openim: invalid API URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("operationID", uuid.NewString())
	if token != "" {
		req.Header.Set("token", token)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return errors.New("openim: API request failed (connection, redirect or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openim: API HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		ErrCode *int            `json:"errCode"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return errors.New("openim: invalid API response")
	}
	if envelope.ErrCode == nil {
		return errors.New("openim: API response missing errCode")
	}
	if *envelope.ErrCode != 0 {
		return &openIMAPIError{*envelope.ErrCode}
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return errors.New("openim: invalid response data")
		}
	}
	return nil
}

func (o *OpenIM) adminToken(ctx context.Context) (string, error) {
	o.tokenMu.Lock()
	defer o.tokenMu.Unlock()
	if o.token != "" && time.Now().Before(o.tokenExpires) {
		return o.token, nil
	}
	var data struct {
		Token             string      `json:"token"`
		ExpireTimeSeconds json.Number `json:"expireTimeSeconds"`
	}
	err := o.post(ctx, "/auth/get_admin_token", "", map[string]string{"secret": o.cfg.AdminSecret, "userID": o.cfg.AdminUserID}, &data)
	if err != nil {
		return "", err
	}
	seconds, err := data.ExpireTimeSeconds.Int64()
	if err != nil || seconds <= 0 || data.Token == "" {
		return "", errors.New("openim: invalid administrator token response")
	}
	// Refresh early; clamp the cache lifetime, not the upstream token lifetime.
	if seconds > 86400 {
		seconds = 86400
	}
	lifetime := time.Duration(seconds) * time.Second
	o.token = data.Token
	o.tokenExpires = time.Now().Add(lifetime - lifetime/10)
	return o.token, nil
}

func (o *OpenIM) api(ctx context.Context, path string, input, output any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := o.adminToken(ctx)
		if err != nil {
			return err
		}
		err = o.post(ctx, path, token, input, output)
		var apiErr *openIMAPIError
		if attempt == 0 && errors.As(err, &apiErr) && (apiErr.Code == 1501 || apiErr.Code == 1502) {
			o.tokenMu.Lock()
			if o.token == token {
				o.token = ""
			}
			o.tokenMu.Unlock()
			continue
		}
		return err
	}
	return errors.New("openim: token refresh failed")
}

// Validate checks credentials and the pre-existing bot without changing OpenIM.
func (o *OpenIM) Validate(ctx context.Context) (string, error) {
	var data struct {
		UsersInfo []struct {
			UserID   string `json:"userID"`
			Nickname string `json:"nickname"`
		} `json:"usersInfo"`
	}
	if err := o.api(ctx, "/user/get_users_info", map[string]any{"userIDs": []string{o.cfg.BotUserID}}, &data); err != nil {
		return "", err
	}
	for _, user := range data.UsersInfo {
		if user.UserID == o.cfg.BotUserID {
			return user.Nickname, nil
		}
	}
	return "", errors.New("openim: bot user does not exist; register an ordinary OpenIM user first")
}
