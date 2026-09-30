package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/channels"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) handleConnectAgentOpenIM(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	uid, aid, ok := s.resolveChannelBindingScope(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req config.OpenIMConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid OpenIM configuration"})
		return
	}
	c, err := channels.NormalizeOpenIMConfig(req)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if _, ok := s.dataStore.(store.ChannelInbox); !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "durable channel ingress unavailable"})
		return
	}
	account := channels.OpenIMAccountID(c)
	// The existing config schema has one channel row per (user, agent, type).
	// Reject replacing a different bot implicitly; disconnect it first.
	previous, lookupErr := s.dataStore.GetConfigByName(r.Context(), store.KindChannel, uid, aid, "openim")
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "could not inspect existing channel"})
		return
	}
	if previous != nil && previous.CredentialKey != account {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": "disconnect the existing OpenIM bot on this agent before connecting another"})
		return
	}
	rows, err := s.dataStore.QueryAllConfigs(r.Context(), store.KindChannel)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "could not inspect OpenIM instances"})
		return
	}
	for _, row := range rows {
		if row.Name != "openim" || (previous != nil && row.ID == previous.ID) {
			continue
		}
		for _, acct := range decodeChannelConfigFromRecord(&row).Accounts {
			if acct.OpenIM == nil {
				continue
			}
			other, err := channels.NormalizeOpenIMConfig(*acct.OpenIM)
			if err == nil && other.APIURL == c.APIURL && (other.AdminUserID != c.AdminUserID || other.AdminSecret != c.AdminSecret) {
				jsonResponse(w, http.StatusConflict, map[string]any{"error": "all bots on the same OpenIM instance must use the same administrator credentials; disconnect existing bindings before rotating credentials"})
				return
			}
		}
	}
	if err := s.assertChannelCredentialUnique(r, "openim", account, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	ch, err := channels.NewOpenIM(c, nil, nil)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	name, err := ch.Validate(ctx)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cc := config.ChannelConfig{Enabled: true, Accounts: map[string]config.AccountConfig{account: {OpenIM: &c}}}
	if err := scope.SaveChannel(r.Context(), s.dataStore, uid, aid, "openim", account, true, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "could not save OpenIM channel"})
		return
	}
	s.invalidateOwner(uid, aid)
	if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), "openim", account); rec != nil {
		s.hotRegisterChannel(*rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "accountId": account, "botName": name, "webhookUrl": openIMWebhookURL(r, c)})
}

func openIMWebhookURL(r *http.Request, c config.OpenIMConfig) string {
	// Reuse the existing reverse-proxy-aware origin, replacing the LINE path.
	origin := strings.TrimSuffix(lineWebhookPathFor(r, ""), "/api/line/webhook/")
	return origin + "/api/openim/webhook/" + channels.OpenIMInstanceID(c) + "/" + channels.OpenIMWebhookSecret(c)
}

func (s *Server) handleOpenIMWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"errCode": 20001, "actionCode": 1})
		return
	}
	type dispatcher interface {
		DispatchOpenIMWebhook(context.Context, string, string, string, []byte) (int, error)
	}
	d, ok := s.userResolver.(dispatcher)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"errCode": 20001, "actionCode": 1})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status, err := d.DispatchOpenIMWebhook(ctx, r.PathValue("instanceId"), r.PathValue("secret"), r.PathValue("command"), body)
	if err != nil {
		jsonResponse(w, status, map[string]any{"errCode": 20001, "actionCode": 1, "errMsg": err.Error(), "errDlt": "", "nextCode": 0})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"errCode": 0, "actionCode": 0, "errMsg": "", "errDlt": "", "nextCode": 0})
}
