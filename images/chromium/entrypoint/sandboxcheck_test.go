package main

import (
	"errors"
	"os"
	"testing"
)

// The check needs unprivileged user namespaces, which some CI machines and
// Ubuntu's restriction refuse to a test binary; it must then fail, not hang.
func TestSandboxCheck(t *testing.T) {
	err := sandboxCheck()
	if err == nil {
		return
	}
	if errors.Is(err, os.ErrPermission) {
		t.Skipf("user namespaces refused here: %v", err)
	}
	t.Logf("sandbox check failed here (fine outside a browser container): %v", err)
}
