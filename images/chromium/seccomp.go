// Package chromium holds what the orchestrator needs to run the browser
// image, ghcr.io/oni-giri/browser-pier-chromium, built from this directory.
package chromium

import _ "embed"

//go:generate go run gen_seccomp.go

// SeccompProfile is the seccomp profile browser containers must run with:
// Docker's default plus the namespace creation Chromium's sandbox needs.
// Under Docker's default profile Chromium can't sandbox pages and won't start.
//
//go:embed seccomp.json
var SeccompProfile []byte
