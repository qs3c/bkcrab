package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/gateway"
)

// Setup receives the CLI adapter as UserResolver, not the underlying Gateway.
// Its optional webhook capability must survive that production wiring.
func TestAPIResolverForwardsOpenIMWebhook(t *testing.T) {
	var resolver api.UserResolver = &apiResolver{gw: &gateway.Gateway{}}
	d, ok := resolver.(interface {
		DispatchOpenIMWebhook(context.Context, string, string, string, []byte) (int, error)
	})
	if !ok {
		t.Fatal("runtime resolver hides OpenIM webhook dispatch")
	}
	status, err := d.DispatchOpenIMWebhook(context.Background(), "instance", "secret", "command", nil)
	if status != http.StatusServiceUnavailable || err == nil || err.Error() != "openim: channels unavailable" {
		t.Fatalf("expected uninitialized gateway response, got %d %v", status, err)
	}
}
