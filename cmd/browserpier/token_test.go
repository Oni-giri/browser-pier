package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTokenCommand(t *testing.T) {
	t.Setenv("BROWSERPIER_DATA_DIR", t.TempDir())
	var out bytes.Buffer
	if err := tokenCommand([]string{"create", "claude"}, &out); err != nil {
		t.Fatal(err)
	}
	secret := strings.SplitN(out.String(), "\n", 2)[0]
	if !strings.HasPrefix(secret, "bp_live_") || !strings.Contains(out.String(), "all profiles") {
		t.Fatalf("create printed %q", out.String())
	}
	out.Reset()
	if err := tokenCommand([]string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "claude") || strings.Contains(out.String(), secret) {
		t.Fatalf("list printed %q", out.String())
	}
	id := strings.Fields(out.String())[0]
	if err := tokenCommand([]string{"create", "x", "-profiles", "nope"}, &out); err == nil {
		t.Fatal("a scope with an unknown profile was accepted")
	}
	if err := tokenCommand([]string{"revoke", id}, &out); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{nil, {"create"}, {"nope"}, {"revoke"}} {
		if err := tokenCommand(bad, &out); err == nil {
			t.Errorf("%v: want usage", bad)
		}
	}
}

func TestPinCommand(t *testing.T) {
	t.Setenv("BROWSERPIER_DATA_DIR", t.TempDir())
	var out bytes.Buffer
	if err := pinCommand(strings.NewReader("482113\n"), &out); err != nil || !strings.Contains(out.String(), "set") {
		t.Fatalf("pin = %v, %q", err, out.String())
	}
	if err := pinCommand(strings.NewReader("12\n"), &out); err == nil {
		t.Fatal("a 2-digit PIN was accepted")
	}
}
