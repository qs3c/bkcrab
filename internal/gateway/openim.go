package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"

	"github.com/qs3c/bkcrab/internal/channels"
)

// DispatchOpenIMWebhook authenticates a server instance, then fans out to its
// bot bindings. The URL secret is deliberately excluded from errors and logs.
func (g *Gateway) DispatchOpenIMWebhook(ctx context.Context, instance, secret, command string, body []byte) (int, error) {
	if g.chanMgr == nil {
		return http.StatusServiceUnavailable, errors.New("openim: channels unavailable")
	}
	var targets []*channels.OpenIM
	knownBots := map[string]bool{}
	for _, ch := range g.chanMgr.Channels("openim") {
		o, ok := ch.(*channels.OpenIM)
		if !ok || o.InstanceID() != instance {
			continue
		}
		knownBots[o.BotUsername()] = true
		if subtle.ConstantTimeCompare([]byte(o.WebhookSecret()), []byte(secret)) == 1 {
			targets = append(targets, o)
		}
	}
	if len(targets) == 0 {
		return http.StatusUnauthorized, errors.New("openim: invalid webhook credentials")
	}
	event, err := channels.ParseOpenIMEvent(command, body)
	if err != nil {
		return http.StatusBadRequest, err
	}
	if knownBots[event.SendID] {
		return http.StatusOK, nil
	}
	for _, ch := range targets {
		if err := ch.AcceptEvent(ctx, event); err != nil {
			return http.StatusServiceUnavailable, errors.New("openim: callback could not be accepted")
		}
	}
	return http.StatusOK, nil
}
