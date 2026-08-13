package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunGatewayProcessesDrainsHTTPBeforeGatewayDependencies(t *testing.T) {
	t.Parallel()
	parent, cancelParent := context.WithCancel(context.Background())
	webStarted := make(chan struct{})
	webCanceled := make(chan struct{})
	releaseWeb := make(chan struct{})
	gatewayStarted := make(chan struct{})
	gatewayCanceled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runGatewayProcesses(parent,
			func(ctx context.Context) error {
				close(webStarted)
				<-ctx.Done()
				close(webCanceled)
				<-releaseWeb
				return nil
			},
			func(ctx context.Context) error {
				close(gatewayStarted)
				<-ctx.Done()
				close(gatewayCanceled)
				return nil
			},
		)
	}()
	select {
	case <-webStarted:
	case <-time.After(time.Second):
		t.Fatal("HTTP runner did not start")
	}
	select {
	case <-gatewayStarted:
	case <-time.After(time.Second):
		t.Fatal("gateway runner did not start")
	}
	cancelParent()
	select {
	case <-webCanceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP admission was not stopped")
	}
	select {
	case <-gatewayCanceled:
		t.Fatal("gateway dependencies stopped before HTTP handlers drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseWeb)
	select {
	case <-gatewayCanceled:
	case <-time.After(time.Second):
		t.Fatal("gateway was not canceled after HTTP drain")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runGatewayProcesses() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("process lifecycle did not finish")
	}
}

func TestRunGatewayProcessesCancelsPeerOnEarlyFailure(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("HTTP failed")
	gatewayCanceled := make(chan struct{})
	err := runGatewayProcesses(context.Background(),
		func(context.Context) error { return sentinel },
		func(ctx context.Context) error {
			<-ctx.Done()
			close(gatewayCanceled)
			return ctx.Err()
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runGatewayProcesses() error = %v, want HTTP failure", err)
	}
	select {
	case <-gatewayCanceled:
	default:
		t.Fatal("early HTTP failure did not cancel gateway peer")
	}
}
