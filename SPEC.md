# BrowserPier — Spec

**Version:** v0.3 · 25 Oct 2026
**Status:** design and decisions done, backend not started
**Repo:** https://github.com/Oni-giri/browser-pier
**Images:** `ghcr.io/oni-giri/browser-pier` (orchestrator) and `ghcr.io/oni-giri/browser-pier-chromium` (browser)
**Tracking:** Jira epic [PROJ-1](https://geistlogic.atlassian.net/browse/PROJ-1)

Self-hosted, persistent browsers for AI agents. Agents start a browser when they need one, it keeps its cookies and logins, and it stops afterwards to free RAM. A human steps in only to log in or clear a 2FA prompt or a captcha.

---

## 1. Goals and non-goals

### Goals

- A real browser reachable from any web browser, natively: no VNC client and no dedicated VM.
- Runs in Docker anywhere: a generic docker-compose for any VPS or server, plus an Umbrel app (the author's own setup).
- About 99% of driving is done by agents. The human only logs in, handles 2FA, solves captchas or takes over when an agent is stuck.
- Two ways to drive:
  - **API mode:** CDP, for Playwright, browser-use and similar.
  - **Visual mode:** screenshots plus OS-level mouse and keyboard, for sites with heavy anti-bot measures.
- Persistent profiles: cookies, logins and local storage survive stops, restarts and updates.
- On-demand browsers: start when an agent asks, stop when idle, so an idle profile uses no RAM.
- A simple web UI and an API to create, list, start, stop and delete profiles and sessions.

### Non-goals

- Multi-user or multi-tenant use. There is one human admin.
- Guaranteed anti-bot bypass. The setup improves the odds; no setup beats every system.
- Fingerprint spoofing. Each profile is a real, stable browser; a fingerprint that changes between sessions is itself a red flag for logged-in accounts.

### Deployment targets

- **Private** (Umbrel or a home server behind Tailscale or a VPN): the safest setup and the recommended one.
- **Public VPS:** supported, with HTTPS required. Expect a datacenter IP, so residential proxies matter much more there.

---

## 2. Why build it rather than use an existing tool

| Option | Good at | Why not, for this use |
|---|---|---|
| Neko | Smooth WebRTC browser in a tab; one long-lived profile. | Built for humans. CDP for agents needs workarounds (config override, policy file, socat relay) that are fragile across image updates. |
| Steel Browser | Agent-first: session API, native CDP, live viewer. | The API server is always on (about 4 GB recommended). Self-hosted profiles share one directory because of an open bug (#347), so separate logins per service aren't possible. One browser per container, headless by default, and both the live viewer and stealth are CDP-based. |

**Decision:** a small custom orchestrator that starts one browser container per session and stops it afterwards. Several ideas from Steel are adopted (see the credits at the end).

---

## 3. Architecture

- **Orchestrator**, always on and lightweight: one Go binary that serves the API, the web UI and an MCP endpoint. It starts and stops browser containers through the Docker SDK and keeps state (profiles, tokens, sessions, handoffs) in SQLite.
- **Web UI:** Go templates with htmx for live updates, embedded in the binary, with no JS build step. Live view and handoff use a small vanilla JS WebRTC client.
- **Browser container**, one per running session: headful Chromium on a virtual display (Xvfb) with no automation flags. It has OS-level input (xdotool) for Visual mode and exposes CDP for API mode. A small sidecar captures the display, encodes H.264 (GStreamer) and sends it to the orchestrator.
- **Live stream:** WebRTC, served by the orchestrator with pion on a single muxed UDP port (TCP fallback). It relays each container's stream to the viewer and passes mouse and keyboard from the viewer back over a data channel to xdotool. No CDP is involved, so Visual mode stays clean.
- **Profile folder**, one per profile: `profiles/<name>/` in the data dir, bind-mounted into the browser container as Chromium's user data directory. It outlives every container, and backing up the data dir backs up every profile. Browser containers run as the orchestrator's own user, so it can read profile files (downloads, export) without a helper container; an orchestrator running as root starts them as uid 1000 instead, since Chromium won't run as root with its sandbox on.
- **Host path:** bind mounts are resolved by the Docker daemon, so the orchestrator needs its data dir's path on the host. When it runs in a container it reads that from its own container's mounts; `BROWSERPIER_HOST_DATA_DIR` overrides it. Before each start the orchestrator writes a marker into the profile folder, and the browser container refuses to start if it doesn't see it, so a wrong path fails loudly instead of opening an empty profile.
- **Proxying:** CDP, the live stream and the visual endpoints are reached only through the orchestrator, behind token checks. Browser containers publish no ports.
- **Control socket:** the container's entry process listens on a Unix socket in a folder shared only with the orchestrator (`profiles/<name>/run/`, so each container sees only its own), never on a network port, so neither pages nor other browser containers can reach it. It relays CDP (headful Chromium binds it to the container's own loopback), serves the H.264 stream and, later, Visual-mode input and screenshots.
- **Sandbox:** Chromium's own sandbox stays on. It needs user namespaces, which Docker's default seccomp profile blocks, so the orchestrator starts browser containers with BrowserPier's seccomp profile: Docker's default plus namespace creation. Containers drop every capability except `SYS_CHROOT`, which the sandbox uses inside its own user namespace, and run with `no-new-privileges`. Never `--no-sandbox`.
- **Chrome launch setup:**
  - Drop `--enable-automation` and set `--disable-blink-features=AutomationControlled`.
  - `--use-angle=swiftshader --enable-unsafe-swiftshader`, so WebGL works in a container (software rendering on SwiftShader; this combination is checked in CI).
  - `--webrtc-ip-handling-policy=disable_non_proxied_udp`, so a proxied profile doesn't leak the host IP.
  - No component updates, background networking or first-run screens.
  - `--password-store=basic`, so cookies decrypt the same way in every container and logins survive image updates.
- **Graceful shutdown:** Chrome is closed cleanly and waited on before its container stops, so recent cookie writes are never lost. The entry process runs as PID 1 and handles stop signals itself. On SIGTERM it closes Chrome through CDP's `Browser.close`, not a signal: Chrome's own SIGTERM handler exits before flushing the cookie store, which loses a login made in the last half minute.
- **Egress:** when a profile has a proxy, a small forwarding proxy in the container adds upstream credentials (Chrome's `--proxy-server` can't take a username and password) and counts traffic. The timezone and locale are looked up from the proxy's exit IP and applied at launch, so the two always agree.

**Capacity:** plan for 1–2 GB per running browser. The author's Umbrel has 16 GB; a typical 4–8 GB VPS fits 2–4 browsers. When a new session wouldn't fit under the RAM limit (a setting, default 70% of host RAM), the orchestrator stops the longest-idle browser that no agent is using. If none can be stopped, it refuses with a clear error listing what is running.

---

## 4. Core concepts

- **Profile:** a saved browser identity with its own cookies and logins, named in lowercase (e.g. `google`). Settings: default mode, auto-stop, screen size, optional proxy, handoff policy.
- **Session:** a running browser on one profile.
- **One session per profile.** Chromium locks its profile, so an agent asking for a profile that is already running attaches to the existing session. Different profiles run in parallel.
- **Modes:**
  - **API (CDP):** fast, but easier for anti-bot to spot.
  - **Visual:** screenshot, then OS-level click and type. Slower, but looks human.

  Each profile has a default mode, and agents can override it per session.
- **Agent token:** one per agent, scoped to all profiles or a chosen list. Shown in the UI as "In use by".

---

## 5. Session lifecycle

| State | Meaning | Main action in the UI |
|---|---|---|
| Starting… | Container and browser booting, a few seconds. | Disabled until ready |
| Running | Browser up; an agent may be attached. | Live view |
| Needs help | An agent requested a handoff and is paused. | Help now |
| Stopping… | Closing the browser, profile flushed to disk. | None |
| Stopped | No container; uses no RAM. Logins kept. | Start |
| Failed | Browser crashed or did not start; last log line shown. | Retry |

- Auto-stop when idle: 5, 10, 15 (default), 30 minutes, or never, set per profile.
- A start takes a few seconds. The UI shows Starting… with a spinner and disables the action until the browser is ready.
- Stopping never loses logins, because the profile is written continuously and Chrome is closed gracefully.

---

## 6. Authentication

- **Admin password** for the web UI:
  - On Umbrel, it's the app password shown in the app's Info.
  - Elsewhere, it's set on a first-run setup page or through `BROWSERPIER_ADMIN_PASSWORD`.
  - Optional "keep me signed in for 30 days".
  - A 5-minute lockout after repeated failures.
  - Cookies are secure-only and HTTP-only.
- **Rate limits** on login, handoff PIN and token endpoints, with failed attempts logged.
- **Agent tokens** for the API and MCP, never the admin password. Created in the UI with a name and a scope, shown once, hashed at rest, and revocable one at a time.
- **Handoff PIN:** 4–6 digits, separate from the admin password, set by the human, hashed. Agents never see it.

---

## 7. Handoff: agent asks a human to take over

1. The agent calls `POST /api/sessions/{id}/handoff` with a reason, a detail (e.g. "Enter the 2FA code") and optionally a stricter `require` level.
2. The session enters Needs help, the agent is paused, and the API returns a one-time URL.
3. The agent sends the URL to the human through its own channel (chat, Telegram, Slack). BrowserPier sends no notifications of its own; the link is the notification.
4. The human opens the link (usually on a phone) and passes the profile's policy check. Control is then theirs, with the agent's reason shown.
5. The human taps **Done, hand back** or **Can't do this**, with an optional note.
6. The agent, long-polling `GET /api/handoffs/{id}`, receives the status and note, then carries on or stops cleanly.

### Handoff policy, per profile

- **Link only:** one tap opens the browser. For low-stakes logins.
- **Link + handoff PIN:** the default.
- **Full sign-in:** admin password required. For email, banking and payments.

### Rules the server enforces

- An agent can raise the level for one handoff, never lower it. Agents browse untrusted pages and can be manipulated through prompt injection, so a lower level asked for by an agent is ignored.
- A link lasts 15 minutes, works once and gives control of that one browser only. It dies as soon as the human hands back.
- Five wrong PIN attempts cancel the link, and the agent is told.
- An expired link sends the human to the normal sign-in.
- On a public instance, "Link only" requires HTTPS and shows a warning when chosen.
- The handoff prompt names only the page's origin (e.g. `accounts.google.com`), never its path or query. Those are controlled by the page and could be used to word a phishing message.
- After hand-back, the agent is told the page may have changed and must re-read it before acting.

### Handoff reasons

Typed so the UI can word the request well, each with a free-text detail:
- `sensitive_input`: login, 2FA, payment
- `captcha`
- `file_upload`
- `review`: check before the agent continues
- `manual_step`

### Handoff over MCP

When the agent is an MCP client that supports URL elicitation (such as Claude), the handoff tool asks the client to show the link and pause the tool call until the human hands back. Clients without it fall back to the plain link plus long-poll flow above.

---

## 8. API

All endpoints take `Authorization: Bearer <agent token>`. The same actions are exposed as MCP tools at `/mcp`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/profiles` | List profiles and their status |
| POST | `/api/profiles` | Create a profile |
| POST | `/api/sessions` | Start or attach to a profile's browser: `{profile, mode}` |
| GET | `/api/sessions` | List running sessions |
| DELETE | `/api/sessions/{id}` | Stop a browser, keep its profile |
| GET | `/api/sessions/{id}/screenshot` | Visual mode: current screen |
| POST | `/api/sessions/{id}/click` | Visual mode: OS-level click `{x, y}` |
| POST | `/api/sessions/{id}/type` | Visual mode: OS-level typing `{text}` |
| POST | `/api/sessions/{id}/computer` | Visual mode: scroll, key, drag, wait, zoom; optional screenshot per action |
| GET | `/api/profiles/{name}/files` | List downloaded files |
| GET | `/api/profiles/{name}/files/{file}` | Fetch a file (DELETE to remove) |
| POST | `/api/sessions/{id}/files` | Upload a file into the session |
| POST | `/api/sessions/{id}/content` | v1.1: page as Markdown, readability text, screenshot or PDF |
| WS | `/cdp/{profile}` | API mode: CDP, proxied and token-checked |
| POST | `/api/sessions/{id}/handoff` | One-time link for a human: `{reason, detail, require?}` |
| GET | `/api/handoffs/{id}` | Long-poll for done / can't, plus the human's note |

Starting a session returns its id, the CDP URL (proxied through the orchestrator) and the live view URL.

### Files

Downloads land in a per-profile folder. The API lists, fetches and deletes them, and uploads files into the session so the agent or a human can pick them in a file chooser. Files are kept with the profile until deleted, or until a size cap evicts the oldest.

### MCP tools

- **Explicit handles:** `start` returns a `session_id` and every stateful tool takes it as an argument. Nothing depends on the MCP connection staying open, so several browsers can run in one conversation.
- **Untrusted content:** every piece of page-derived text returned to an agent (snapshots, markdown, page titles) is handled the same way:
  - wrapped in an untrusted-content fence that says it is data, not instructions
  - stripped of invisible Unicode and HTML comments
  - the values of password and one-time-code fields are redacted
- **Visual-mode actions:** click, type, scroll, key, drag and wait, each optionally returning a screenshot. Screenshots are downscaled server-side to 1280×720, and a zoom action crops a region at full resolution.

---

## 9. Web UI

Designs: the "Browser Orchestrator UI" canvas (private to the author). The UI still says "Browser Pool"; rename it to BrowserPier when building.

| Screen | What it must do |
|---|---|
| 1 · Login | Admin password, keep signed in, lockout message, help text pointing to where the password lives (Umbrel app Info, or the first-run setup). |
| 2 · Profiles | All profiles with status, default mode, in use by, memory, activity and auto-stop. Host RAM meter, needs-help badge, ⋯ menu (copy CDP URL, stop, edit, export, delete), stop and delete confirmations, empty state. |
| 3 · New profile | Name, handoff policy, "start and log in now"; Advanced holds mode, auto-stop, screen size and proxy. |
| 4 · Live view | Streamed browser, take or hand back control, controls locked while the agent drives, connection details, agent activity log, help request. |
| 5 · API access | Agent tokens (create with scope, shown once, revoke), handoff PIN and rules, quick start, endpoint list, MCP. |
| 6 · Handoff (phone) | PIN entry, take control on a phone (tap to click, pinch, Type, Fit), note, Done or Can't do this, then expired and finished states. |

### Interaction rules

- Needs help is visible from anywhere in the UI: an amber status, sorted to the top, and a header badge with a count.
- Red is reserved for destruction. Stop is neutral; Delete sits in the ⋯ menu and asks for confirmation, naming what will be erased.
- Stop warns when an agent is using the browser.
- Each row has one main action in a fixed position (Help now, Live view, Start, Retry). Everything else is in the ⋯ menu.
- Browser controls are disabled while the agent drives. While the human drives, the keyboard goes to the browser and Esc releases it.
- "Stop browser" is the label everywhere. Copy actions confirm with "Copied".
- Nautical theme in the logo and a few touches of copy; functional labels stay plain.

---

## 10. Anti-bot approach

- Headful Chromium on a virtual display, with no automation flags.
- Visual mode sends real OS-level input through xdotool, not CDP input events. The CDP connection itself is detectable, so in Visual mode no CDP client stays attached to the page.
- Residential IP: a home server's own connection by default, with an optional per-profile proxy. On a VPS, a residential proxy is close to required for protected sites.
- Timezone and locale always match the exit IP.
- WebRTC can't leak the host IP when a proxy is set.
- No fingerprint spoofing: each profile keeps one real, consistent fingerprint over time.
- Persistent, aged profiles with real logins help on their own.

---

## 11. Security

- Private by default: behind Tailscale or a VPN is the recommended setup. Public instances must use HTTPS.
- The orchestrator holds the Docker socket, which is powerful. Keep it minimal, and only ever start containers from BrowserPier's own browser image.
- Raw CDP has no authentication, so it is never exposed directly; it goes through the token-checked proxy.
- CDP access is equivalent to full control of every logged-in account in that profile. Docs say so plainly, and tokens are scoped per profile.
- Tokens, the PIN and the admin password are hashed at rest.
- Treat profile folders as secrets, since they hold live session cookies. With `password-store=basic`, cookies on disk are effectively readable.
- Page text returned to agents is fenced and sanitised (see MCP tools), since agents browse untrusted pages.

---

## 12. Deployment and packaging

- **Generic (primary):** a `docker-compose.yml` with the orchestrator and an optional Caddy service for automatic HTTPS, or instructions for an existing reverse proxy. Configured through environment variables, with install docs for a fresh VPS.
- **WebRTC on a server:** advertise the public IP (or the Tailscale IP), open one UDP port with a TCP fallback. Optional TURN (v1.1) for viewers on networks that block UDP.
- **Umbrel:** an app in the author's community app store, with the manifest plus a compose using Umbrel's app proxy and app password.
- Browser containers are started by the orchestrator, not listed in the compose file.
- App data holds the database and one folder per profile.
- The orchestrator listens on port **7437** by default ("PIER" on a phone keypad), set with `BROWSERPIER_LISTEN`. It avoids 8080 and other ports common among self-hosted apps, since Umbrel needs a unique port per app.
- **Profile export and import:** from a profile's ⋯ menu, download it as a password-encrypted archive, and import one into a new profile. A profile must be stopped to export, so its files are consistent.

---

## 13. Decisions

| Question | Decision | Why |
|---|---|---|
| Live stream | WebRTC (pion), H.264 from the virtual display | Smooth, and no CDP attached to the page, so Visual mode stays hard to detect. |
| Orchestrator stack | Go, single binary, SQLite | Low idle RAM, native Docker SDK and pion. |
| Frontend | Go templates + htmx, small vanilla JS for WebRTC | No build step; everything ships in the one binary. |
| Notifications | None from BrowserPier | The agent already messages the human with the handoff link. |
| RAM limit | Stop the longest-idle unused browser, else refuse | Agents get a browser when possible and a clear error when not, instead of hanging in a queue. |
| Profile backups | Encrypted export and import in the UI | Losing the server shouldn't mean logging in everywhere again. |
| Deployment | Generic docker-compose first (any VPS), Umbrel app second | The author runs it on Umbrel, but it should work for anyone on a plain server. |
| Fingerprints | No spoofing; one stable real fingerprint per profile | Spoofing needs CDP on every page, and a changing fingerprint flags logged-in accounts. |
| Proxies | Per-profile, authenticated, via a forwarding proxy; timezone and locale matched | On a VPS, residential proxies are what makes protected sites usable. |
| Orchestrator ↔ browser container | A Unix socket in a per-profile folder shared with the orchestrator | Nothing listens on a network, so a page or another container can't reach CDP. |
| Profile storage | Bind-mounted folders in the data dir, not named Docker volumes | One directory to back up or move; export and downloads read files directly. The host path is detected from the orchestrator's own mounts. |
| Chromium sandbox | On, with a seccomp profile that allows user namespaces | Pages are untrusted; the container is a second boundary, not the only one. |

### Still open

- **Logo:** navy style (v1.1).
- **License:** Apache 2.0 (most adoption, same as Steel and Kernel) or AGPL (a cloud host must share changes). Decide before the first public release.

---

## 14. Roadmap

### v1: everything needed to run it daily, privately or on a VPS

- Browser image with the Chrome launch setup, basic cookie store and graceful shutdown
- Orchestrator core, auth with rate limits, agent API, WebRTC live view
- Auto-stop and RAM limit
- Handoff links with reasons, origin-only prompts and MCP elicitation
- Files: downloads and uploads
- Per-profile proxies with credentials, timezone and locale matching
- MCP endpoint with explicit session handles, untrusted-content fencing and Visual-mode actions
- Web UI (6 screens), encrypted profile export/import
- Generic docker-compose with HTTPS (Caddy), first-run setup, Umbrel app
- Anti-bot validation

### v1.1: efficiency and reach

- Content endpoints: page to Markdown or readability text, screenshot, PDF (API mode)
- Token-efficient API-mode tools: accessibility snapshot with stable refs, find, batch, covered-element check
- TURN support for viewers on UDP-blocking networks
- Seed a profile from your own Chrome: cookie and localStorage JSON export/import, so you don't have to log in again inside BrowserPier
- Navy-style logo

### Later: useful, not urgent

- MP4 recording of each session from the existing H.264 stream, for replay
- Automatic login-wall and CAPTCHA detection that triggers a handoff

### Beyond v1: productization (vision, not committed)

v1 stays fully self-hosted on one box. The aim right now is to ship a complete, solid FOSS product, not to compete with hosted providers or chase uniqueness; good ideas from other projects are welcome. This section is where BrowserPier could go afterwards. Nothing here is scheduled; it exists so v1 doesn't close doors.

**Groundwork that's cheap in v1**
- Put the browser lifecycle behind a `Runner` interface (start, stop, and optionally suspend/resume, for a profile) with local Docker as the only v1 backend.
- Put profile storage behind a `ProfileStore` interface with a local folder as the only v1 backend.
- Both are internal Go interfaces: no new features, no new config.

**Stage 1: FOSS adoption**
- One-line install; templates for Umbrel, CasaOS, Unraid and Coolify.
- Python and TypeScript SDKs; ready-made integrations with browser-use (the first example: it connects over CDP), Stagehand and Playwright.
- Docs and example agents ("log into X once, then automate it").
- Pick the license before the first public release (see Still open).

**Stage 2: teams and several machines** (likely the first paid tier)
- Postgres instead of SQLite; a distributed lock so "one running browser per profile" holds across machines.
- `ProfileStore` on S3-compatible storage (S3, R2, MinIO): profile packed, encrypted and uploaded on stop, restored on start. Chrome caches (Cache, Code Cache, GPUCache) are left out so snapshots stay small and cold starts fast.
- `Runner` backends for remote Docker hosts and Kubernetes.
- Multiple users with roles, SSO (OIDC), an audit log, profiles shared within a team.
- Team handoff queue: "google needs a human", whoever is free takes it; push notifications or a small mobile app.

**Stage 3: hosted product**
- Billing per browser-hour plus profile storage; regions; bring-your-own or provided proxies.
- Freeze/resume browsers from memory snapshots instead of stop/start: an idle browser costs disk only and resumes in milliseconds with its tabs open. Candidates: Unikraft Cloud (what Kernel uses for headful Chromium) or Firecracker on our own bare metal (needs KVM).
- Abuse prevention from day one: sign-up checks, rate limits, domain blocklists. Hosted browsers attract spam and fraud.
- Trust: profiles encrypted with customer-held keys, EU hosting and data residency.

**Ideas that grow out of the core** (not a race with anyone; just what persistent profiles and handoffs make possible)
- Profile health: detect when a profile has been logged out and ask a human to log in again before an agent hits the wall.
- Password manager integration (Bitwarden, 1Password) for re-logins without exposing secrets, with domain-bound secrets: a credential can only be typed on the domains it's allowed for.
- Session recordings and an audit trail of who, agent or human, did what in which account.
- Handoff as a product: the phone and PIN flow, team routing, response-time stats.

**Business model:** open core. The self-hosted single box stays free; team features (Stage 2) paid or source-available; a hosted cloud for people who don't want to run it.

**Landscape to watch:** Browserbase (hosted), Steel (open source, Apache 2.0), Kernel (hosted on Unikraft; its browser images are open source under Apache 2.0, with headful Chromium and a WebRTC live view), Cloudflare Browser Run (hosted; every request carries signed bot-identification headers, so it doesn't suit anti-bot use), browser-use (an open-source agent library that drives any CDP browser, so a client of BrowserPier rather than an alternative; its hosted Browser Use Cloud keeps cookies and localStorage per profile, syncs logins from a local browser and has a live preview).

---

### Credits

Several ideas come from studying [Steel Browser](https://github.com/steel-dev/steel-browser) and [Steel's MCP server](https://github.com/steel-dev/steel-mcp-server), both Apache 2.0:
- the Chrome launch flags
- cookie-store handling
- timezone matching
- the forwarding proxy
- MCP handoff via elicitation
- untrusted-content fencing
- token-efficient tools

Reused snippets keep their attribution.
