package motion

import "time"

// ShortcutGap is the least time between an agent's key call that ends on
// a browser-bound shortcut (ctrl+l, alt+d, F6…) and its next keyboard
// action, in Visual mode (PROJ-151). While a page has the keyboard,
// Chrome moves the focus to its own UI only once the page has answered
// the shortcut's keys, so text sent sooner reached a page whose key
// handler is busy; measured on Chromium 156, 150 ms covers a handler that
// takes up to about 100 ms a key (images/chromium/ANTI-BOT.md). It is a
// floor, not a guarantee.
//
// The browser image's entrypoint waits it out (shortcutGap in
// images/chromium/entrypoint/visual.go, at most 250 ms); the press_keys
// tool's description and the Docker tests in internal/api read it here,
// so the value lives in one place.
const ShortcutGap = 150 * time.Millisecond
