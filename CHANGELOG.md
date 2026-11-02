# Changelog

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
