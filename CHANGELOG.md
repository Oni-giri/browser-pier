# Changelog

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
