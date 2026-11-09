package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestAddressesSettled(t *testing.T) {
	for _, c := range []struct {
		name, ifInet6 string
		want          bool
	}{
		{"no IPv6", "", true},
		{"still in duplicate address detection",
			"fe800000000000009825a4fffe9b96c2 02 40 20 c0     eth0\n00000000000000000000000000000001 01 80 10 80       lo\n", false},
		{"detection done",
			"fe800000000000009825a4fffe9b96c2 02 40 20 80     eth0\n00000000000000000000000000000001 01 80 10 80       lo\n", true},
		// An address whose detection failed stays tentative for good.
		{"detection failed", "fe800000000000009825a4fffe9b96c2 02 40 20 c8     eth0\n", true},
		{"garbled", "fe80 02 40\n", true},
	} {
		if got := addressesSettled(c.ifInet6); got != c.want {
			t.Errorf("%s: addressesSettled = %t, want %t", c.name, got, c.want)
		}
	}
}

// Only a timeout lets startup go on without the network settled: a
// display that exited or a stop doesn't.
func TestWaitForTellsATimeoutApart(t *testing.T) {
	never := func() bool { return false }
	running := &process{done: make(chan struct{})}
	if err := waitFor(context.Background(), running, 50*time.Millisecond, never); !errors.Is(err, errNotReady) {
		t.Errorf("timeout: %v", err)
	}
	exited := &process{done: make(chan struct{})}
	close(exited.done)
	if err := waitFor(context.Background(), exited, time.Second, never); err == nil || errors.Is(err, errNotReady) {
		t.Errorf("exited: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitFor(ctx, running, time.Second, never); err == nil || errors.Is(err, errNotReady) {
		t.Errorf("stopped: %v", err)
	}
}

func TestSettleNetworkGoesOnOnlyAfterATimeout(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	never := func() bool { return false }
	settled := func() bool { return true }
	running := &process{done: make(chan struct{})}
	if err := settleNetwork(context.Background(), log, running, 50*time.Millisecond, never); err != nil {
		t.Errorf("timeout: %v", err)
	}
	if err := settleNetwork(context.Background(), log, running, time.Second, settled); err != nil {
		t.Errorf("settled: %v", err)
	}
	exited := &process{done: make(chan struct{})}
	close(exited.done)
	if err := settleNetwork(context.Background(), log, exited, time.Second, never); err == nil {
		t.Error("Xvfb exited, and startup went on")
	}
	// A stop that comes as the network settles still wins.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, ready := range map[string]func() bool{"waiting": never, "settled": settled} {
		if err := settleNetwork(ctx, log, running, time.Second, ready); err == nil {
			t.Errorf("stopped while %s, and startup went on", name)
		}
	}
}
