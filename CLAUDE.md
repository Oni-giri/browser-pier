# CLAUDE.md — working on BrowserPier

Read `SPEC.md` first. It is the source of truth for behaviour, decisions and scope. If code and spec disagree, raise it; don't silently diverge. Update the spec when a decision changes.

## Task tracking (Jira)

All work is tracked in Jira project **PROJ** on `geistlogic.atlassian.net`, under epic **PROJ-1**. Every BrowserPier task has the label `browserpier` plus one of `v1`, `v1.1` or `later`. The board is shared across the author's projects, so always filter by the `browserpier` label.

If the Atlassian MCP is connected:
- Before starting a task, move it to **In Progress**.
- While working, comment on notable decisions or blockers.
- When it's done, comment with a short summary (what was built, how to test it, any follow-ups) and move it to **Done**.
- Create a new task (with the right labels, under PROJ-1) for follow-up work you discover, rather than widening the current one.

If it isn't connected, use the list below and mention ticket keys in commits.

### Build order (v1)

| Key | Task |
|---|---|
| PROJ-2 | Initial commit: SPEC.md, README, CLAUDE.md, .gitignore *(done in this commit)* |
| PROJ-3 | Scaffold the Go module and repo layout, Makefile, CI |
| PROJ-4 | Browser container image (Chromium + Xvfb + xdotool + stream sidecar, launch flags, graceful shutdown) |
| PROJ-5 | Orchestrator core (Docker SDK, profiles, session lifecycle, SQLite) |
| PROJ-6 | Auth (admin login, agent tokens, handoff PIN, rate limits) |
| PROJ-7 | Agent API (profiles, sessions, CDP proxy, visual endpoints) |
| PROJ-17 | Files (downloads and uploads) |
| PROJ-18 | Per-profile proxies (credentials, timezone and locale matching) |
| PROJ-9 | Auto-stop on idle + RAM limit |
| PROJ-8 | WebRTC live view (pion relay, input data channel, JS client) |
| PROJ-10 | Handoff links (policy, PIN, long-poll, expiry, reasons, origin-only) |
| PROJ-11 | Web UI (Go templates + htmx, 6 screens) |
| PROJ-12 | MCP endpoint (session handles, elicitation, untrusted-content fence, visual tools) |
| PROJ-13 | Encrypted profile export/import |
| PROJ-19 | Generic deployment (docker-compose, Caddy HTTPS, first-run setup, install docs) |
| PROJ-14 | Umbrel app (community app store) |
| PROJ-15 | Anti-bot validation |

**v1.1:**
- PROJ-20: content endpoints
- PROJ-21: token-efficient API-mode tools
- PROJ-22: TURN
- PROJ-24: seed a profile from your own Chrome (cookie and localStorage JSON export/import)
- PROJ-16: logo

**Later:**
- PROJ-23: MP4 recording
- PROJ-25: automatic CAPTCHA detection

Don't start v1.1 or later work until the v1 tasks it depends on are done.

## Stack and layout

- **Go** for the orchestrator: a single binary with SQLite, the Docker SDK and pion for WebRTC.
- **Go templates + htmx** for the UI, embedded in the binary. No JS build step; only a small vanilla JS file for the WebRTC client.
- Layout:
  - `cmd/browserpier/`: the orchestrator's entry point
  - `internal/`: packages such as `docker`, `sessions`, `profiles`, `auth`, `api`, `webrtc`, `ui`, `handoff`, `mcp`, `proxy`, `files`
  - `images/chromium/`: the browser container image (Dockerfile, entrypoint, stream sidecar)
  - `deploy/`: the generic docker-compose and Caddy config
  - `umbrel/`: the Umbrel app manifest and compose
- Configuration goes through environment variables, prefixed `BROWSERPIER_`.

## Non-negotiable rules

- Browser containers publish **no ports**. CDP, the stream and the visual endpoints are reached only through the orchestrator, behind token checks.
- **In Visual mode, no CDP client stays attached to the page.** Input goes through xdotool and the stream comes from the X display.
- **No fingerprint spoofing.** Keep one real, stable browser per profile.
- Launch Chromium with `--password-store=basic` and close it **gracefully** before stopping its container, so logins aren't lost.
- One running session per profile; a second request attaches to the existing one.
- Agents can **raise** a handoff's security level but never lower it. Handoff links are single-use, last 15 minutes and are limited to one browser.
- Every piece of page-derived text returned to an agent goes through the untrusted-content fence:
  - wrap it in the fence
  - strip invisible Unicode and HTML comments
  - redact password and OTP field values
- Hash tokens, the PIN and the admin password. Never log them.
- The orchestrator only ever starts containers from BrowserPier's own browser image.

## Working conventions

- Small, focused commits that mention the ticket key, e.g. `PROJ-5: session lifecycle state machine`.
- Write tests alongside code. Docker-dependent tests go behind a build tag so `go test ./...` stays fast without Docker.
- Before calling a task done, run it for real: start a browser container, log into a test site, stop it, start it again and confirm the login survived.
- Prefer the standard library and a few well-known dependencies.
- Code reused from Steel (Apache 2.0) keeps its attribution, plus a NOTICE entry.
