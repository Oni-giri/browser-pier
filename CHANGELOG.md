# Changelog

## 0.2.0

**Before you update:** the compose file now needs **Docker Compose 2.20 or newer** (`docker compose version`). Browsers started by 0.1.x are closed gracefully on the first start, so logins are kept, and start again under the new definition when next used.

**New**
- **Page tools for agents in API mode,** over MCP and REST, so an agent that only speaks MCP can read and use a page without pulling in raw HTML or a screenshot:
  - `snapshot`: the page's controls with short refs, and its text, within a budget.
  - `find`: text on the page, with the nearest ref.
  - `act`: 1–20 steps on refs, such as click, fill, type, press, select, check and hover.
  - `wait_for`: wait for text, a ref, or a URL change.
  - `navigate` and `tabs`.

  A whole login is about 2,000 characters, about half of one screenshot. Everything they return is fenced as untrusted page content. A field that looks secret shows only `[filled]` or `[empty]`, never its value. They add nothing to the page and never switch on a CDP domain, so detection scripts don't see them. REST: `POST /api/sessions/{id}/snapshot`, `find`, `act`, `wait`, `navigate` and `tabs`.
- **Page content:** `POST /api/sessions/{id}/content`, and `page_content` over MCP. It returns the active tab as Markdown, as reader-view text (the main article only), as a screenshot, or as a PDF. Long pages come in parts. Markdown and reader view are rendered by BrowserPier from what the browser lays out, so no script runs in the page, and hidden text and form values are left out. A PDF is a print, which the page can see.
- **TURN for the live view and phone handoffs,** for viewers on networks that block UDP or direct connections, such as some corporate Wi-Fi and mobile carriers:
  - Point BrowserPier at your own TURN server with `BROWSERPIER_TURN_URLS` and `BROWSERPIER_TURN_SECRET`. Each viewer then gets its own expiring login.
  - Or start the bundled coturn with `--profile turn`. It relays only to BrowserPier, over UDP, and only port 3478 is published.
  - See "TURN" in `deploy/README.md`.
- **BrowserPier's own AppArmor profile,** `browserpier`. On Debian 13, umbrelOS 2 and other AppArmor 4 hosts, Docker's profile can break Chromium's sandbox, so since 0.1.2 browsers there ran without AppArmor. They now keep a profile: Docker's own, plus user namespaces.
  - With compose, add `--profile apparmor` to `docker compose up` on Debian 13 or later. It loads the profile once as root and installs it so the host keeps it across reboots.
  - On Umbrel it loads whenever the app starts.
  - BrowserPier checks which profile works and picks the best one. If none of its own is loaded, it falls back as before and says so in its log.
- **A desktop in the browser:** a small window manager with a taskbar. Pages now see a work area smaller than the screen, as on a real desktop, instead of the bare screen headless browsers report. Screenshots and the live view show the taskbar along the bottom.

**Fixed**
- Stopping a browser could answer "502" and mark the session as crashed when the host failed to tear down its network afterwards, as rootless Podman 5 does on Ubuntu. The browser had closed normally. A stop that leaves the container stopped now counts.
- The bundled coturn no longer leaves an anonymous Docker volume behind each time it's recreated.

## 0.1.5

**New**
- Dark mode. Settings → Appearance picks System, Light or Dark, for each device. System follows the device's own setting. The sign-in and phone handoff pages follow the choice too, from the first paint.
- Quick start has an Agent tab: a two-line message to paste into any agent's chat. It points the agent to `/mcp/setup`, a guide with this BrowserPier's address filled in that tells it how to connect itself, for Claude Code, Codex and other clients, with the token read from `PIER_TOKEN` and never written into a config. Clients without MCP get the REST API.
- BrowserPier is under the Apache 2.0 license. Both images carry LICENSE and NOTICE.

**Fixed**
- A new browser must also fit in what the host has free, other apps included, not just under the memory limit. When it doesn't, the longest-idle unused browser is stopped first, as for the limit. The refusal says which one ran short, for example "the host has 0.8 GB free, other apps included, and a browser needs about 1.0 GB". Before, a host short because of other apps could swap, or have its OOM killer end an app or a browser mid-task.
- An agent's first page load could fail with `ERR_NETWORK_CHANGED` when it was still loading a second or two after the browser started. Chromium now starts once the container's network has settled, which makes a start about 1.4 s longer.
- On a phone, the handoff's live view fits the screen instead of scrolling sideways.
- A long toast, such as a memory refusal, was squeezed into half the screen's width. It now takes the width it needs, up to 640px.

## 0.1.4

**New**
- A new look, following the design: a sky-blue page, slate greys and a header that stays on top as you scroll. Orange now only means someone is needed: while a profile waits for you, the page light turns warm, the "needs you" chip pulses and the first waiting profile stands out, with "Take control" in blue. When nobody is waiting, the page goes back to blue by itself. The sign-in and phone handoff pages follow the design too.
- Profiles show their site's icon. After a browser stops gracefully, BrowserPier reads the main site's icon from the browser's own icon cache, the files the browser itself wrote. It treats those files as untrusted, and the icon is redrawn before it is shown. New profile and Edit have a "Main site" field. Left empty, it is guessed from the site the browser visits most. Under Advanced you pick a fallback icon and colour for profiles whose site has none. Agents keep their initials.
- Agents can give a new profile its `site`, `icon_glyph` and `icon_color` when they create it. Exports carry them, and the icon.

**Fixed**
- With two profiles waiting, the chip said "2 needs you". It now says "2 need you".

## 0.1.3

**Fixed**
- Pages load faster, most of all behind a proxy like Umbrel's: the stylesheet, scripts and icon were fetched again on every page. They are now linked by version and kept by the browser until they change.
- Memory estimates count what other apps use. Overview shows their share in the memory bar, and Overview, Profiles and Settings say how many more browsers fit in what the host has free, when that is less than the memory limit leaves. The limit itself works as before.
- A profile's ⋯ menu on its own page offers Export and Delete, and Edit while its browser runs, instead of repeating the buttons beside it.
- The Agents page says agents can connect over MCP, as well as the REST API and CDP.

## 0.1.2

**Fixed**
- Browsers wouldn't start on Debian 13 hosts, umbrelOS 2.0 among them: Chromium aborted at startup. Docker's default AppArmor profile refuses Chromium's sandbox there, so on those hosts browser containers now run without an AppArmor profile. Ubuntu keeps Docker's profile, which works there, and so does a host whose admin set Docker's default profile. Every other restriction stays, and so does Chromium's sandbox. When BrowserPier reaches Docker through `DOCKER_HOST` it can't check the host, so it keeps the profile and says so in its log. `BROWSERPIER_BROWSER_APPARMOR` names a profile to use instead, `unconfined` included.
- A browser that fails to start now says why: Chromium's own error is shown with the failure, on the profile page and to agents, who get it inside the untrusted-content fence.
- On a phone, the header no longer wraps the account menu onto a row of its own. The logo, the "needs you" chip and the account menu share the first row, and the navigation sits below them. Between 640 and 1000px the menu no longer drops to a second row either.
- On a phone, no page scrolls sideways any more: Profiles, Agents and Settings each had something wider than the screen.
- The account avatar keeps its colour while its menu is open.

## 0.1.1

**Fixed**
- Signing in, and unlocking a handoff with its PIN, failed over plain HTTP on any address but localhost (a LAN address, `umbrel.local`, a Tailscale address) with "cross-origin request detected". These pages' referrer policy made the browser send `Origin: null` on their forms.

## 0.1.0

The first release: persistent, self-hosted browsers for AI agents.

**Browsers and profiles**
- One real Chromium per profile, in its own container, with logins that survive stops, restarts and updates. Chromium is always closed gracefully before its container stops.
- Browsers start when an agent asks and stop after a profile's idle time. When memory runs short, the longest-idle unused browser is stopped first.
- Per-profile proxies, with credentials. A profile's time zone and language follow its exit location.
- Downloads and uploads, kept per profile under a size cap.
- Pages can't reach the local network, the Docker host or a cloud metadata endpoint. The admin can allow one profile the local network.

**Agents**
- REST API with scoped agent tokens. One running browser per profile: a second request attaches to it.
- **API mode:** CDP through BrowserPier, for Playwright and similar. Clients that can't set headers connect with a one-minute, single-use ticket from `POST /api/sessions/{id}/cdp-ticket`.
- **Visual mode:** screenshots plus OS-level clicks, typing, scrolling and keys, with no CDP client attached to the page.
- MCP endpoint at `/mcp`, with the same tools. Page text returned to agents is fenced and cleaned.
- **Handoff links:** an agent asks a person to finish a login, a 2FA prompt or a captcha. Links are single-use, last 15 minutes and are protected by a PIN when the policy asks. Agents can raise a link's security level but not lower it.

**You**
- Web UI for profiles, agents, tokens and settings, with a live view of any browser over WebRTC, where you can take control.
- Encrypted export and import of a profile.

**Install**
- docker-compose for any Linux server, with optional Caddy for HTTPS. See `deploy/README.md`.
- An Umbrel app for a community app store, in `umbrel/`.

**Known limits**
- The repository is private for now, so installing needs a GitHub sign-in for the files and `docker login ghcr.io` for the images. See `deploy/README.md`.
- Pages get no QUIC, and WebRTC in pages runs over TCP only, since every connection goes through the browser's filtering proxy.
- WebGL is software-rendered (SwiftShader), which some bot detection notices. See `images/chromium/ANTI-BOT.md`.
