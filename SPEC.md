# BrowserPier — Spec

**Version:** v0.4 · 12 Nov 2026
**Status:** v1 shipped (0.1.5); v1.1 next (see Jira, label `v1.1`)
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
- **Browser container**, one per running session: headful Chromium on a virtual display (Xvfb) with no automation flags, on a minimal desktop: a window manager (JWM) and a taskbar. It has OS-level input (xdotool) for Visual mode and exposes CDP for API mode. A small sidecar captures the display, encodes H.264 (GStreamer) and sends it to the orchestrator.
- **Live stream:** WebRTC, served by the orchestrator with pion on a single muxed UDP port (TCP fallback). It relays each container's stream to the viewer and passes mouse and keyboard from the viewer back over a data channel to xdotool. No CDP is involved, so Visual mode stays clean.
  - **Ports and addresses:** `BROWSERPIER_WEBRTC_PORT` (default 7438) is the one UDP port, and the same TCP port is the fallback; `BROWSERPIER_WEBRTC_IPS` sets the addresses to advertise (a VPS's public IP, the Tailscale IP). The live view is the web UI's profile page, `/profiles/{profile}`, for the signed-in admin (`/live/{profile}` redirects there).
  - **TURN (optional):** for viewers on networks that block UDP or direct connections (corporate Wi-Fi, some mobile carriers). `BROWSERPIER_TURN_URLS` names the servers. A viewer gets them, with credentials, from `/live/{profile}/ice` (the signed-in admin) or `/h/{token}/ice` (the browser holding an open handoff link), behind the same checks as the offer, never before.
    - **Credentials:** with `BROWSERPIER_TURN_SECRET`, the TURN REST API's shared secret, each viewer gets its own. Their username's id is a keyed hash of the viewer (the admin's signed-in browser, or the handoff), the same on every request, so the TURN server's per-user quota holds one viewer however often it asks, and the id says nothing of the session or link. They open no new relay after 12 hours for the admin, or a minute after the link expires for a handoff. coturn checks them only when a relay is set up, so an open relay outlives them until it's closed; BrowserPier closes a handoff's live view when the handoff ends. The secret never leaves the orchestrator, and neither it nor a password is logged. A static login (`BROWSERPIER_TURN_USERNAME`, `BROWSERPIER_TURN_PASSWORD`) is for services without the REST API.
    - **The orchestrator doesn't relay:** viewers' relayed candidates reach its own port. `BROWSERPIER_TURN_PEER_IPS`, the address a TURN server reaches it at (the compose network's, for the bundled coturn), is advertised besides `BROWSERPIER_WEBRTC_IPS` while TURN is on.
    - **Checking it:** `?ice=relay` on the profile page, or on a handoff link's page once it's open, connects through TURN only.
    - Decided 2026-11-12 (PROJ-22).
  - **Streaming:** a browser's stream is read only while someone watches, so an unwatched browser spends nothing on encoding. Each frame is one sample; x264 writes one slice per frame.
  - **Control:** a person takes control to send mouse and keyboard. It is refused while an agent is driving: connected over CDP, or having clicked or typed in the last 5 seconds. While a person has control, agents can only watch: their input and new CDP connections are refused, and open ones close. Esc releases the keyboard; leaving the page or losing focus lets go of any held key or button.
  - **Input safety:** input reaches the browser as structured events (move, button, wheel, key by X keysym name; printable characters as Unicode keysyms, so the viewer's own layout types), fed to one long-running `xdotool`. Every command is built in the container from checked values; nothing a viewer sends becomes script text.
  - **Phone:** tap to click, pinch to zoom, Type and Fit buttons.
  - **Idle:** a browser someone is watching is never stopped as idle.
- **Profile folder**, one per profile: `profiles/<name>/` in the data dir, bind-mounted into the browser container as Chromium's user data directory. It outlives every container, and backing up the data dir backs up every profile. Browser containers run as the orchestrator's own user, so it can read profile files (downloads, export) without a helper container; an orchestrator running as root starts them as uid 1000 instead, since Chromium won't run as root with its sandbox on.
- **Host path:** bind mounts are resolved by the Docker daemon, so the orchestrator needs its data dir's path on the host. When it runs in a container it reads that from its own container's mounts; `BROWSERPIER_HOST_DATA_DIR` overrides it. Before each start the orchestrator writes a marker into the profile folder, and the browser container refuses to start if it doesn't see it, so a wrong path fails loudly instead of opening an empty profile.
- **Proxying:** CDP, the live stream and the visual endpoints are reached only through the orchestrator, behind token checks. Browser containers publish no ports.
- **Control socket:** the container's entry process listens on a Unix socket in a folder shared only with the orchestrator (`profiles/<name>/run/`, so each container sees only its own), never on a network port, so neither pages nor other browser containers can reach it. It relays CDP (headful Chromium binds it to the container's own loopback), serves the H.264 stream and, later, Visual-mode input and screenshots.
- **Sandbox:** Chromium's own sandbox stays on. It needs user namespaces, which Docker's default seccomp profile blocks, so the orchestrator starts browser containers with BrowserPier's seccomp profile: Docker's default plus namespace creation. Containers drop every capability except `SYS_CHROOT`, which the sandbox uses inside its own user namespace, and run with `no-new-privileges`. Never `--no-sandbox`.
  - **AppArmor:** Docker's `docker-default` AppArmor profile has no `userns` rule. Under AppArmor 4 without Ubuntu's user namespace restriction (Debian 13, umbrelOS 2.0), whether that refuses the sandbox its user namespace depends on Docker's profile template. Docker 29.1 and earlier (moby/profiles apparmor v0.1.x) write no ABI line, so the host's AppArmor 4 parser mediates user namespaces, the profile refuses them, and Chromium aborts (`credentials.cc:137 … Permission denied (13)`). Docker 29.9 (v0.2.x) pins the profile to `abi <abi/3.0>,`, which doesn't mediate user namespaces, and the sandbox works under it. Both were seen on Debian 13.5 (kernel 6.12, AppArmor 4.1); versions in between are unchecked. So BrowserPier ships its own profile, `browserpier` (`images/chromium/apparmor/`): moby's `docker-default` (moby/profiles apparmor v0.2.0's template), pinned to `abi <abi/4.0>,`, plus `userns,` and `deny pivot_root,`. ABI 4.0 is the first that mediates user namespaces and the last where `network,` still covers unix sockets; under it, POSIX message queues are mediated too, and refused. io_uring isn't mediated before ABI 5.0; the seccomp profile refuses it.
    - **Loading it** takes root on the host, which the orchestrator doesn't have. The compose file's `apparmor` service (the browser image, as root with only `CAP_MAC_ADMIN`, unconfined, no network, the host's `/sys/kernel/security` and `/etc/apparmor.d` mounted) runs once at each `docker compose --profile apparmor up -d`: it loads the profile and installs it in `/etc/apparmor.d`, so the host loads it at boot. It's opt-in, for Debian 13 and later, because Compose can't even create it where those host paths are missing or can't be mounted (rootless Docker or Podman, hosts without AppArmor, Docker Desktop), and `up` would fail. BrowserPier depends on it with `required: false`, so it starts even when loading fails; the loader also refuses early, with a clear message, when it isn't root in the host's user namespace. On Umbrel, `hooks/pre-start` loads it with the host's own parser at every start of the app, a reboot included, without installing it. The hook carries the profile and the loader itself, since Umbrel's app update copies only the app files it knows (`hooks/` among them, not an `apparmor/` folder). Both load it only where it may be needed: the kernel mediates user namespaces (Ubuntu's kernels say so with a `features/namespaces/userns_create` file, Debian's with that word in `features/namespaces/mask`), the host's AppArmor is 4.0 or later, and Ubuntu's restriction is absent. They load it whatever Docker's version: the check then prefers it. The admin can also load it by hand (deploy/README.md, "AppArmor").
    - **Choosing** at start, unless `BROWSERPIER_BROWSER_APPARMOR` names a profile: where the daemon reports its stock AppArmor profile (`name=apparmor`, or `name=apparmor,profile=default` in newer Docker, not a default the admin set, nor a rootless daemon) and the kernel has no `kernel.apparmor_restrict_unprivileged_userns`, or the daemon is remote (`DOCKER_HOST` set, even to a unix socket, which may be forwarded), the orchestrator runs the browser image's sandbox check in a short-lived container with the browsers' exact settings: a child in new user, PID and network namespaces that then chroots, which takes a capability inside its namespace, as Chromium's sandbox does. It tries `browserpier`, then Docker's default (AppArmor 3, Debian 12, or Docker 29.9's ABI 3.0 profile), then `unconfined`, and uses the first that works; when that isn't `browserpier`, the log line says why `browserpier` failed. The check container runs as the browsers' user, is labelled, has no network or mounts, takes 30 s at most, and is removed after. The browser image says it has the check with the label `io.browserpier.sandbox-check`; an older image is reported as such.
    - **Only a real answer settles the choice.** An option fails only if the daemon couldn't apply the profile or the check itself failed. Anything else (the daemon erring, a timeout, the image missing) leaves the choice unsettled: browsers use the fallback for now, and the check runs again before the next browser start. The orchestrator never pulls the browser image at start; without it, the first browser start pulls it and checks. While unsettled, Reconcile keeps browsers running under any AppArmor choice, so a hiccup doesn't stop them.
    - **Unloaded later** (`apparmor_parser -R`, or a host restart where the profile isn't kept in `/etc/apparmor.d`; on Debian 13 `systemctl reload` or `restart apparmor` leaves it loaded, even when it isn't in `/etc/apparmor.d`; umbrelOS is unchecked): a browser start that can't apply the chosen profile unsettles the choice, and the manager tries once more, which checks again. Unloading a profile with `apparmor_parser -R` also leaves the processes it confined unconfined, at once, and loading it again doesn't confine them again: running browsers stay unconfined until they restart, while their label still says `browserpier`.
    - **Not loaded** isn't fatal: browsers run unconfined, with a warning that says how to load the profile, as before BrowserPier had its own. Refusing would break every Debian 13 server updated without the new compose service. If nothing works, an error is logged and browsers run unconfined (local) or under Docker's default (remote). Everything above (seccomp, capabilities, no-new-privileges) applies whatever the choice.
    - Ubuntu is the other way round: with that restriction, an unconfined process gets a user namespace without the capabilities the sandbox needs, while `docker-default` works. There, and wherever that sysctl exists, browsers keep Docker's profile, unchecked.
    - Containers' definition label includes the AppArmor choice, so a browser started under another choice is stopped when BrowserPier restarts, not picked up.
    - `BROWSERPIER_BROWSER_APPARMOR` names a profile to use instead, unchecked. A named profile that isn't loaded fails each browser start with an error saying so.
    - Decided 2026-11-03 (PROJ-35: unconfined where Docker's profile refuses the sandbox) and 2026-11-12 (PROJ-37: BrowserPier's own profile, checked at start).
- **Chrome launch setup:**
  - Drop `--enable-automation` and set `--disable-blink-features=AutomationControlled`.
  - `--use-angle=swiftshader --enable-unsafe-swiftshader`, so WebGL works in a container (software rendering on SwiftShader; this combination is checked in CI).
  - `--webrtc-ip-handling-policy=disable_non_proxied_udp`, so WebRTC sends no UDP, which would go around the forwarder (see Egress) and, behind a proxy, leak the host IP.
  - No component updates, background networking or first-run screens.
  - `--password-store=basic`, so cookies decrypt the same way in every container and logins survive image updates.
  - The window sits at 0,0 with no frame and fills the screen above a 32-pixel taskbar along the bottom, so Visual-mode coordinates are screen coordinates. The taskbar shows a clock only: no menus, key bindings or window buttons, so nothing on the desktop starts a program or kills Chromium, and the window can't be moved off 0,0.
- **Graceful shutdown:** Chrome is closed cleanly and waited on before its container stops, so recent cookie writes are never lost. The entry process runs as PID 1 and handles stop signals itself. On SIGTERM it closes Chrome through CDP's `Browser.close`, not a signal: Chrome's own SIGTERM handler exits before flushing the cookie store, which loses a login made in the last half minute.
- **Egress:** every connection Chromium makes goes through a small SOCKS5 forwarder in the container (`--proxy-server`, with `--proxy-bypass-list=<-loopback>` so loopback and link-local addresses don't skip it). Chromium resolves no name itself (`--host-resolver-rules`), DNS-over-HTTPS is off by policy, and there is no DIRECT fallback: if the forwarder or the proxy is down, pages fail rather than going out directly.
  - **The local network is off limits:** pages are untrusted, so the forwarder refuses private addresses (the Docker host through the bridge, its other containers, the LAN; `10/8`, `172.16/12`, `192.168/16`, `100.64/10` where Tailscale lives, `fc00::/7`) and the host's own addresses as `BROWSERPIER_WEBRTC_IPS` gives them. It always refuses loopback, link-local (the cloud metadata endpoint `169.254.169.254`) and addresses that aren't one host. IPv6 addresses that carry an IPv4 one (mapped, NAT64, 6to4) are judged by it.
  - **Without an upstream proxy** it resolves names itself, refuses a name if any of its addresses is refused, and connects to the addresses it checked, so a second DNS answer can't swap in a private one.
  - **Opt-in:** a profile can let its pages reach the local network, for a router's admin page, say. Only the admin sets it, in the web UI; agents can't, through the API. Loopback and link-local stay refused.
  - **With an upstream proxy** (HTTP, HTTPS or SOCKS5, with a username and password), the forwarder adds the credentials (Chrome's `--proxy-server` can't take them) and counts traffic. Host names go on to the upstream proxy to resolve, on its own network, so there only addresses and `localhost` names are checked.
  - The credentials reach the container as a file in its control folder that the entrypoint reads and deletes before Chromium starts, never as an environment variable. The orchestrator removes it when the session ends, in case it was never read.
  - **Limits:** the forwarder stops pages, not a compromised browser process, which still has the bridge; and the host's public address is only known when the admin sets it. Network-level isolation is PROJ-31 (later).
- **Time zone and language match the exit:** before Chromium starts, the entrypoint asks where it exits, through the proxy if there is one (`BROWSERPIER_GEOIP_URL`, an echoip-style JSON endpoint; default `https://ifconfig.co/json`, which sees each exit IP; self-host echoip to avoid that). Chromium then runs with the matching `TZ`, `LANGUAGE`/`LANG` and an Accept-Language list a person there would send (e.g. `de-DE,de,en-US,en`).
  - The location is stored on the profile. While the country stays the same, the stored time zone is kept, so a rotating proxy doesn't move a logged-in account between zones. Use sticky proxy sessions where the provider offers them.
  - A profile can set its time zone and language by hand, which wins.
  - If the lookup fails, the stored location is used, with a warning. A proxied profile with nothing stored doesn't start: the error says to set the time zone and language by hand. Without a proxy it starts anyway, in UTC and English, with a warning.

**Capacity:** plan for 1–2 GB per running browser. The author's Umbrel has 16 GB; a typical 4–8 GB VPS fits 2–4 browsers. A new session must fit under the RAM limit (a setting, default 70% of host RAM) **and** in what the host has free, other apps included (its MemAvailable, less what browsers still booting will take). When it doesn't, the orchestrator stops the longest-idle browser that no agent is using. If none can be stopped, it refuses with a clear error saying which ran short and listing what is running, e.g. "the host has 0.8 GB free, other apps included, and a browser needs about 1.0 GB". The host is checked with no RAM limit too (`BROWSERPIER_RAM_LIMIT_PERCENT=0`), and not when BrowserPier reaches Docker through `DOCKER_HOST`, whose host may be another machine. Attaching to a running browser is never refused. Browsers' memory is read from Docker (as `docker stats` counts it), and a new one is expected to need 1 GB. An agent's refusal names only the browsers its token can see; the full list goes to the log. Starting is checked one at a time, so two starts can't both squeeze in. The web UI's estimates ("room for about N more", "fits about N browsers") also count what other apps leave: the host's MemAvailable plus what the browsers use. It shows other apps' share too (PROJ-42); the limit itself counts only BrowserPier's browsers, and the host check covers the rest (PROJ-45).

---

## 4. Core concepts

- **Profile:** a saved browser identity with its own cookies and logins, named in lowercase (e.g. `google`). Settings: default mode, auto-stop, screen size, optional proxy, handoff policy, local network access (off).
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
  - **What counts as use:** any agent API call on the session (start or attach, screenshot, click, type, upload, each API-mode page tool: snapshot, find, act, wait_for, navigate, tabs, and each content call). A page tool's call keeps the browser busy while it runs (a 30-second `wait_for` too), but it holds no connection between calls, so a browser driven only by the page tools still stops when idle.
  - **In use the whole time:** an open CDP connection, and a browser a person has during a handoff, are never idle.
  - **Stopping:** an idle browser is stopped gracefully, so its logins are kept.
- A start takes a few seconds. The UI shows Starting… with a spinner and disables the action until the browser is ready.
- **Orchestrator restarts:** browsers keep running while the orchestrator restarts, and it picks them up again. Each container is labelled with the version of its definition (`io.browserpier.spec`); a browser from an older one, such as one started before pages went through the egress filter, is stopped the usual way instead, logins kept, and the next start gets a current one.
- Stopping never loses logins, because the profile is written continuously and Chrome is closed gracefully.
- **A stop Docker reports as failed** still counts when the container has in fact stopped: it is logged as a warning, and the exit status decides Stopped or Failed as usual. Rootless Podman on Ubuntu does this when AppArmor keeps it from stopping `pasta` (PROJ-64). If the container is still running, the session goes back to the state it was in and the error is returned.

---

## 6. Authentication

- **Admin password** for the web UI:
  - On Umbrel, it's the app password shown in the app's Info.
  - Elsewhere, it's set on a first-run setup page or through `BROWSERPIER_ADMIN_PASSWORD`.
  - The environment variable wins at every start, so changing it is also how to reset a forgotten password; it signs out every browser. The UI then can't change the password.
  - The setup page asks for a one-time code that the orchestrator prints in its log, so whoever first reaches a fresh public instance can't claim it. It disappears once a password is set.
  - Optional "keep me signed in for 30 days"; otherwise the session ends with the browser, and after a day at most.
  - A 5-minute lockout after 5 failures from one address in 5 minutes. Past 20 failures (and attempts in flight) from anywhere in 15 minutes, sign-ins from unknown devices go one at a time, each after a delay that starts at 2 seconds and doubles with every 20 more failures, up to 30 seconds. Browsers that have signed in before carry a signed device cookie (valid a year, voided by a password change, distrusted after 5 failures) and skip this, so an attack from elsewhere never slows the owner's usual devices. A lockout never signs out existing sessions.
  - Cookies are HTTP-only and `SameSite=Lax`, and `Secure` whenever BrowserPier is served over HTTPS (or `BROWSERPIER_PUBLIC_URL` is https). Private plain-HTTP setups (Umbrel, Tailscale) drop `Secure`, because browsers refuse a Secure cookie there.
  - Each sign-in gets a fresh session; changing the password signs out every other browser. Cross-site form posts are refused.
- **The cookie and the token never mix:** the cookie authenticates the UI only, agent tokens the API and MCP only.
- **Client addresses:** `X-Forwarded-For` and `X-Forwarded-Proto` are believed only from `BROWSERPIER_TRUSTED_PROXIES` (CIDRs, or `private`); none by default. The shipped compose files set it for their proxy.
- **Rate limits** on login, setup, handoff PIN and token endpoints, with failed attempts logged (address and outcome, never the secret).
- **Agent tokens** for the API and MCP, never the admin password. Created in the UI with a name and a scope, shown once, hashed at rest (SHA-256 of 256 random bits, prefixed `bp_live_`), and revocable one at a time. A token scoped to a list loses a profile when it is deleted.
- **Handoff PIN:** 4–6 digits, separate from the admin password, set by the human, hashed (argon2id, like the admin password). Agents never see it. A short PIN's hash can be cracked offline in seconds, so the attempt limits, not the hash, are what protect it.

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
- A handoff that needs the PIN is refused (409) while no handoff PIN is set; the agent can ask for `require=signin` instead.
- Wrong PINs are also capped per address (10), per profile across its links (15) and across all profiles (30), each over 15 minutes, since the PIN is shared.
- An expired link sends the human to the normal sign-in.
- On a public instance, "Link only" requires HTTPS and shows a warning when chosen.
- The handoff prompt names only the page's origin (e.g. `accounts.google.com`), never its path or query. Those are controlled by the page and could be used to word a phishing message.
- After hand-back, the agent is told the page may have changed and must re-read it before acting.
- **How "works once" is enforced:** the first browser to pass the policy check gets a cookie only it holds (stored hashed); any other browser is refused, even with the PIN.
- **Handoff links only work over HTTPS, except from private addresses** (the LAN, Tailscale): opened over plain HTTP from outside, a link asks for no PIN or password and opens nothing, and says it needs HTTPS.
- **While the link is open:** the person drives the live view from the start, and the agent is paused: its connections close and its input is refused until hand-back, expiry or cancellation.
- **If the browser stops:** an open handoff is cancelled.

### Handoff reasons

Typed so the UI can word the request well, each with a free-text detail:
- `sensitive_input`: login, 2FA, payment
- `captcha`
- `file_upload`
- `review`: check before the agent continues
- `manual_step`

### Handoff over MCP

`request_handoff` opens the handoff, then:
- **Clients that can open URLs for their user** (URL elicitation, such as Claude) are asked to show the link. The tool call waits for the hand-back, up to 4 minutes at a time, then says it's still pending so the agent calls `wait_for_handoff`.
  - If the client declines the link (headless clients, such as `claude -p`, do so without asking anyone) or fails to show it, the link stays open and the agent gets it to pass on itself, as below. The agent can call `cancel_handoff` if nobody will take it.
  - The endpoint keeps MCP sessions (the 2025-11-25 protocol), since asking the client to show a link needs one. A client on the session-less 2026-07-28 protocol falls back to it.
- **Other clients** get the link to pass on themselves, and wait with `wait_for_handoff`.
- **The same rules as the API apply:** a PIN-policy handoff is refused while no handoff PIN is set.

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
| POST | `/api/sessions/{id}/computer` | Visual mode: one action `{action, …, screenshot?, width?}`: click, type, scroll, key, drag, wait or zoom |
| GET | `/api/profiles/{name}/files` | List downloaded files |
| GET | `/api/profiles/{name}/files/{file}` | Fetch a file (DELETE to remove) |
| POST | `/api/sessions/{id}/files` | Upload a file into the session |
| POST | `/api/sessions/{id}/snapshot` | API mode: the page's URL, title, controls with refs and text, within a budget `{max_chars?}` |
| POST | `/api/sessions/{id}/find` | API mode: text on the page, with context and the nearest ref `{text, limit?}` |
| POST | `/api/sessions/{id}/act` | API mode: 1–20 steps on refs `{steps, snapshot?}` |
| POST | `/api/sessions/{id}/wait` | API mode: wait for text, a ref visible or hidden, or a URL change `{text? \| ref, state? \| url_change, timeout_ms?}` |
| POST | `/api/sessions/{id}/navigate` | API mode: open a URL, or go back, forward or reload `{url? \| action}` |
| GET, POST | `/api/sessions/{id}/tabs` | API mode: list tabs, or switch, close or open one `{action, id?, url?}` |
| POST | `/api/sessions/{id}/content` | The active tab as Markdown or its main text (API mode), a screenshot (either mode) or a PDF (API mode) `{format, …}` |
| WS | `/cdp/{profile}` | API mode: CDP, proxied and token-checked |
| POST | `/api/sessions/{id}/cdp-ticket` | API mode: a one-minute, single-use CDP URL for clients that can't set headers |
| POST | `/api/sessions/{id}/handoff` | One-time link for a human: `{reason, detail, require?}` |
| GET | `/api/handoffs/{id}` | Long-poll for done / can't, plus the human's note |
| DELETE | `/api/handoffs/{id}` | Withdraw a request the agent no longer needs: `{note?}`. Refused (409) once a person has opened the link. |

Starting a session returns its id, the CDP URL (proxied through the orchestrator) and the live view URL.

- **Scopes:** a token sees only the profiles it is scoped to; anything else answers 404, as if it didn't exist. Only a token for all profiles can create one.
- **Main site and icon:** creating a profile takes `site`, the main site whose favicon is the profile's icon in the web UI (a hostname such as `mail.google.com`, at most 253 characters; a URL is cut down to its host), and `icon_glyph` and `icon_color`, the fallback icon (see Profile icons in §9). Profiles are listed with them. The site BrowserPier infers from the browser's history isn't returned (it is read from pages, and says which sites a profile uses), and the icon itself is served only to the admin.
- **What agents can't change:** an agent creating a profile can't set its proxy (a proxy sees where the browser goes, and an agent can be steered by the pages it reads) or open the local network to its pages, and can make its handoff policy stricter than the default but never looser.
- **CDP:** `/cdp/{profile}` is the browser's WebSocket; `/cdp/{profile}/json/version` and `/json/list` work too, with their URLs pointing back through BrowserPier, so `connectOverCDP("https://…/cdp/{profile}")` works. The token never reaches the browser.
  - **Clients that can't set headers** get a ticket from `POST /api/sessions/{id}/cdp-ticket`, with the token in the header, and connect to the `cdp_url` it returns (`wss://…/cdp/{profile}?ticket=…`), as a WebSocket or as the `http(s)` URL Playwright fetches `/json/version` from.
  - **A ticket** opens one profile's CDP endpoint once, within 60 seconds, as the token that made it. It's kept hashed in memory only, and revoking the token voids it.
  - **The `/json` endpoints** use up the ticket they were called with, and the URLs they return each carry a fresh one, so a client can follow them. Calling `/json/version` and then `/json/list` takes two tickets.
  - **The token is never taken in the URL** (`?token=` answers 400): a proxy in front may log URLs, and a ticket that leaks there is already used or about to expire. The proxy refuses a Visual-mode session (no CDP client may attach to the page) and a browser a person is using during a handoff.
- **Visual mode:** screenshot, click and type go through the container's X display and xdotool. During a handoff, agents can still take screenshots but not click or type. Coordinates are screen pixels, and the screen includes the 32-pixel taskbar along the bottom; a screenshot can be scaled down with `?width=`, and the response gives the screen size.
- **Computer actions:** `/computer` takes one action. `click {x, y, button?, double?}` and `type {text}` work as above; `scroll {x, y, dx?, dy?}` scrolls by whole wheel notches at a point, at most 20 each way; `key {keys}` presses a combination like `"ctrl+l"` or a list of up to 10, each made of X keysym names (`Return`, `Tab`, `a`…, with aliases such as `Enter`, `Esc`, `Cmd`, `PageDown`) joined by `+`; `drag {from: {x, y}, to: {x, y}, button?}` presses, moves in steps and releases. These run through xdotool with the same checks as click, never as an xdotool script. `wait {ms}` pauses up to 10 s. `zoom {x, y, w, h}` returns that region of the screen as a PNG at full resolution, cut off at the screen's edges. With `screenshot: true` any other action answers with the screen afterwards (scaled to `width` if given) instead of 204. During a handoff, wait and zoom still work. The activity log names each action but never typed text: keys that could spell something are logged only as a count.
- **Tokens without the UI:** `browserpier token create NAME [-profiles a,b]`, `token list` and `token revoke ID` manage tokens from the command line, e.g. through `docker exec`.

### API-mode page tools

For agents that speak only MCP (or REST), six tools read and act on an API-mode page in a few hundred tokens, without raw HTML or a screenshot (PROJ-21). They answer `{"text": "…"}`, the same text the MCP tools return, and refuse a Visual-mode session (409). Another profile's session is 404, as everywhere.

- **How they reach the page:** each call opens one short-lived CDP connection from the orchestrator through the container's control socket, never through the agent proxy, and closes it before answering. It sends only the methods on the tools' allow-list (`internal/pagetools`), never a domain's `enable` or `DOM.getDocument`, runs its scripts only in an isolated world, and writes nothing into the page; `fill` and `select` set values as Playwright does. The agent's own CDP client can be connected at the same time.
- **Lifecycle:** a call takes a page lease on the session: one call at a time per session, refused while a person has the browser, cancelled by a handoff, a stop, the token's revocation or a session hold (which waits for it to end). A lease doesn't show the agent as connected, but like a click it keeps a person from taking control while it runs and for 5 seconds after.
- **Refs** like `a7e12` name an element of one document: a 2-character epoch, random per session map (so refs from before an orchestrator restart answer stale), then a number never reused. A ref goes stale when its document changes, its node goes, or a person had the browser since; a ref from a tab that isn't active is refused. At most 5,000 are live per session.
- **The active tab** is the browser's foreground tab, the one whose page is visible: `tabs switch` and `new` bring theirs to the front, so the tools, the live view and a handoff see the same one. When a page opens a tab (a sign-in popup), every tool's result says `a new tab opened: t3 (https://accounts.example)`, and the agent switches to it.
- **snapshot** joins the accessibility tree and a DOM snapshot of the top frame (an iframe is one line, not read: its origin, or its address when that has none, quoted if it has whitespace or a quote in it, so a page string can't start a line of its own). It spends its budget (4,000 characters by default, at most 20,000) on an open modal, then what is in view, then the main landmark, then the rest, controls before text in each, sums up a navigation or menu of more than 20 controls in one line, and says what it left out. Secret fields (as `internal/fence` judges them) show only `[filled]` or `[empty]`. A page with more than 60,000 DOM nodes is too large for the accessibility tree: `snapshot` says so, and `find` reads its text alone.
- **act** runs 1–20 steps in order, stopping at the first failure: `click`, `fill`, `type`, `press`, `select`, `check`, `uncheck`, `hover`. Pointer steps check that nothing covers the element's centre (its own label may), typing steps that it has the focus before and after. Each result names the element's role and name as they are when it acts; typed text is only counted. `fill` and `type` refuse date, time, range, colour and file inputs; `click` refuses a file input (or its label), whose chooser is a window of the browser's own that nothing over CDP could close. Clicking a date input opens Chrome's picker, which `press Escape` closes. A page's own button that opens a file chooser can't be told apart; the chooser then waits for a person (a handoff). The page can still change between a check and the input; act resolves, checks and dispatches back to back and echoes what it hit, and promises no more.
- **wait_for** polls every 250 ms, for up to 30 s. **navigate** takes only `http` and `https` URLs without a user name or password, and waits up to 30 s for the page to load.
- **Isolated worlds, exactly:** code never runs in a world by its context number, which a cross-process navigation can make name a page's own world. Each call reads the world's `globalThis` once, keeps it only if the frame's document didn't change meanwhile, and runs everything as functions on it, or on nodes checked to be in it.
- **Memory:** one answer from the browser is capped at 32 MiB, and at most two whole-page reads (a DOM snapshot and an accessibility tree) run at once across all sessions, so large pages can't take a small host's memory; a read waits at most 10 seconds for a slot, and a handoff or stop still ends the wait. A page read keeps at most 4 MB of the page's text (find searches all of it, whole blocks included); when a page has more, find says some was left out.
- **Answers:** 200 with the text; 400 for a bad request; 401 when the token is revoked during the call; 404 for a session the token can't see; 409 when the session is in Visual mode, a person has the browser, a ref is stale or in another tab, an element is covered or loses the focus; 422 for an input `act` doesn't handle, or a page too large to read; 502 when the browser refuses or can't be reached (its words fenced); 503 when other sessions' large pages have held both read slots for 10 seconds (try again shortly: a call waiting for one holds its session, so it doesn't wait longer); 504 when the page stops answering (a dialog, most likely).
- **Dialogs:** without `Page.enable` an `alert()` can't be seen or closed over CDP, so a call that the page stops answering while the browser still does says "the page is busy or has a dialog open; if it stays that way, request a handoff."
- **The activity log** records the action and refs only (`Clicked a7e12`, `Filled a7e13 (12 characters)`), never page text or typed text.

### Page content

`POST /api/sessions/{id}/content` returns the active tab (the page tools' active tab) in one of four formats, for an agent that wants a whole page or article rather than its controls (PROJ-20). MCP has the same as `page_content`.

| `format` | Mode | Answer | Options |
|---|---|---|---|
| `markdown` | API | `{"text": "…"}`: the whole page as Markdown (headings, lists, quotes, code, simple tables, links and images with absolute addresses), fenced | `max_chars` (default 20,000, at most 100,000), `offset` |
| `readability` | API | `{"text": "…"}`: the main text only, as plain text without links, as a reader view shows it; the fewest tokens | the same |
| `screenshot` | both | `image/png` of the screen, as `/screenshot` | `width` |
| `pdf` | API | `application/pdf`, as an attachment, never rendered; over MCP an embedded `application/pdf` resource | `landscape`, `background`, `pages` (like `"1-5, 8"`) |

- **How Markdown and the main text are made:** one DOM snapshot (`DOMSnapshot.captureSnapshot`) through the same kind of per-call connection as the page tools, with its own allow-list (`internal/pagetools`: targets, frame tree, isolated world for finding the active tab, the DOM snapshot and `Page.printToPDF`; no input, navigation or DOM writes). The snapshot is turned into text in Go (`internal/pagetext`); no script of BrowserPier's runs to read the page. The tab's document is checked before and after: a snapshot that straddles a navigation is taken again once, then refused.
- **What is left out:** what the snapshot shows isn't rendered (`display:none`, `visibility:hidden`, zero-size, scripts, styles, templates, SVG), form fields with their values, however they were filled (a password never comes this way; read controls with `snapshot`), and iframes' documents (one `[iframe: origin]` line). Text a person may not see but that is laid out (transparent, off-screen, clipped) stays, fenced, as in a snapshot; so does `aria-hidden` text, which a person sees but `snapshot` (reading the accessibility tree) leaves out. Shadow roots are read where they are shown. An element with no box of its own but shown content (`display:contents`) keeps its role: a heading, a link, a table row.
- **Links** keep absolute `http`, `https` and `mailto` addresses of up to 512 characters; a link to a script, to this page's fragment, or with a user name in it (`https://good.example@evil.example/`, as `navigate` refuses) keeps only its text. A link around blocks (a card on a front page) has its blocks, then its address on a line of its own. Page text can't make a heading, list, quote, fence or table row (it is escaped), and a `<!--` in it is written `<\!--`, so it can't hide the text after it from whoever renders the Markdown (the fence does the same).
- **The main text** (`readability`) follows reader views' heuristics, written for BrowserPier rather than ported: navigation, headers and footers outside the article, sidebars, forms, dialogs, buttons and blocks whose class or id names a banner, cookies, comments, sharing… (as words, so `cookie_notice`, `siteFooter` and `has-sidebar` count; never a block holding an `<article>` or `<main>` whose own class isn't one of these, so a comment's `<article class="comment-body">` doesn't count) go; a page with one `<article>`, else one `<main>`, has its main text there; otherwise blocks are scored on their paragraphs' length and commas and penalised for link text. Link lists in the result are dropped, unless the page is mostly links (a news front page). A word like content or post doesn't save a block named for comments, a footer or a sidebar (`post-comments`, `footer-content`). One retry, for a wrapper whose class misled it (`<div id="community-page">`, `<div class="layout has-sidebar">`): when the page has no article or main and one block left out by its class held most of the page's text (more than half when under 200 characters was found, 80% whatever was found), it is found again with that block kept, and taken if it keeps all that was found first, or joined with it in the page's order when the two don't overlap. Comments, footers and banners are never taken for wrappers. Forum threads, where every post is a block of its own, come back as one post (PROJ-92). A page where nothing stands out comes back whole, cleaned.
- **Length:** the text is cut at `max_chars` characters, at a line's end when one is near, and the line after the fence says `Characters 0 to 20,000 of 54,321. Read on with offset 20000.` A part that starts inside a code block opens it again. BrowserPier renders at most 2 MB of text and 200,000 DOM nodes of a page, 256 levels deep (deeper elements are their text alone, after a note saying so), in time and memory linear in the page. The DOM snapshot sends a string once however many nodes share it, so a page can give thousands of nodes the same megabyte; BrowserPier reads at most 256 bytes of a class or id, 1 KB of other attributes, 2 KB of an address, and 8 MB of shown text from the whole page, so that costs nothing either. Text that isn't shown, or is a script's, a style's or a field's, spends none of it, so hidden megabytes can't push the article out; text in navigation, sidebars, forms and dialogs has 2 MB of its own. When a page has more shown text than that, the line after the last part's fence says so, in both formats; beyond that it keeps what fits and says so, or answers 422 (`page too large to read; use find`). A content call takes one of the whole-page read slots the page tools share (two at once, for every session) and holds it while it renders or prints: at most about 30 seconds (the snapshot's 20, rendering's 10) for text, 60 for a PDF, and a handoff, a stop or the caller leaving ends it at once. When the slots stay taken for 10 seconds it answers 503 (busy, try again).
- **Screenshots** come from the X display, in both modes, with no CDP at all: the same picture as `/screenshot` and the `screenshot` tool, browser window included. `Page.captureScreenshot` would give the page alone and a full-page capture, but a full-page capture resizes the page, which it can see.
- **PDF** is Chromium's print (`Page.printToPDF`), at most 10 MiB (ask for fewer `pages` beyond it) and 60 seconds. The page sees it print, as when a person presses ctrl+P (see Anti-bot approach). A PDF, like a screenshot, is the page as it is printed or shown: it holds form fields' values in clear (a password field shows dots, but a one-time code or a typed address doesn't), and what the page's CSS shows only in print.
- **Visual mode:** `markdown`, `readability` and `pdf` answer 409 before anything connects; `screenshot` works. During a handoff only `screenshot` works, as for `/screenshot`.
- **The activity log** says `Read the page as Markdown`, `Read the page's main text`, `Took a screenshot` or `Printed the page to PDF (120 kB)`, never page text.

### Files

Downloads land in a per-profile folder. The API lists, fetches and deletes them, and uploads files into the session so the agent or a human can pick them in a file chooser. Files are kept with the profile until deleted, or until a size cap evicts the oldest.

- Each profile has `files/downloads` and `files/uploads`, mounted at `/files` in its browser. A managed Chromium policy sends every download to `/files/downloads` without asking, in both modes, with no CDP involved.
- An uploaded file lands in `/files/uploads/<name>` (the response gives the path): an agent sets it on a file input with `DOM.setFileInputFiles`, and a person can pick it in the file chooser. Uploads are at most 100 MiB.
- The cap is per profile, downloads and uploads together (`BROWSERPIER_FILES_LIMIT_MB`, default 1024); the oldest files go first, and a download in progress is never evicted.
- Downloads come from untrusted pages, so the API always serves them as attachments (`application/octet-stream`, `nosniff`, a sandbox CSP), never rendered.
- The browser can put anything in these folders, symlinks included. The orchestrator only touches them through `os.Root` and only serves regular files, so a planted link can't expose a host file.

### MCP tools

- **Endpoint:** `/mcp` speaks MCP over streamable HTTP and takes an agent token like the API. Each tool calls the API in-process with the caller's token, so scopes, refusals and the agent log are the API's. An MCP session belongs to the token that opened it.
- **Setup guide:** `GET /mcp/setup` is a Markdown page for agents, with this BrowserPier's address filled in, that tells an agent how to connect itself. It covers checking `PIER_TOKEN` (without printing it), the setup for each common client with the token read from the variable at connect time, never written into a config, checking with `list_profiles`, and the REST API as the fallback without MCP. It holds no secret, so it needs no sign-in; under `/mcp/` Umbrel lets it through.
- **Tools:**
  - `list_profiles`, `start_browser`, `stop_browser`
  - Visual mode: `screenshot`, `click`, `type_text`, `press_keys`, `scroll`, `drag`, `wait`, `zoom`
  - API mode: `snapshot`, `find`, `act`, `wait_for`, `navigate`, `tabs` (see API-mode page tools)
  - `page_content`: Markdown, the main text or a PDF in API mode, a screenshot in either (see Page content)
  - `list_files`
  - `request_handoff`, `wait_for_handoff`, `cancel_handoff`
- **Explicit handles:** `start_browser` returns a `session_id` and every stateful tool takes it as an argument. Nothing depends on the MCP connection staying open, so several browsers can run in one conversation.
- **Untrusted content:** every piece of page-derived text returned to an agent (downloaded file names, the page tools' snapshots, matches, step results and tab names, errors that quote the page, and the page as Markdown or main text) is handled the same way:
  - wrapped in an `<untrusted-content-…>` fence whose tag name carries a random marker, so the page can't write the closing tag, saying it is data, not instructions (look-alike tags in the text are defused)
  - stripped of invisible Unicode (zero-width, bidi, tag characters), with HTML comments defused rather than dropped: `<!--` is written `<\!--`. What the fence wraps is page text, file names, logs and errors, never HTML, so a `<!--` in it is text a person sees; dropping it hid everything after it, and defused it can't hide text from a person reading a transcript that renders HTML or Markdown either (PROJ-95). A caller that ever fences raw HTML drops its comments itself, by parsing it.
  - password and one-time-code field values redacted (`internal/fence`), at the source where the output is structured (a snapshot shows such a field as `[filled]`). Redaction prevents accidental leaks, not leaks by a manipulated agent: a code split across several boxes, one shown as page text (a webmail profile) or a token in a URL still reaches the agent.
- **Visual-mode actions:**
  - Each action can return a screenshot after it.
  - Screenshots are downscaled server-side to fit 1280×720 (1440×900 becomes 1152×720). Tools take coordinates in the screenshot's pixels, and BrowserPier scales them to the screen.
  - `zoom` returns a region at full resolution.
  - Responses are kept short: one line of text, plus the image where there is one.

---

## 9. Web UI

Designs live in the repo: `design/DESIGN.md` (tokens, rules, components, routes), `design/screens/` (HTML, open with `python3 -m http.server`) and `design/screenshots/`. The editable originals are on the author's private design canvas.

| Screen | What it must do |
|---|---|
| 1 · Login | Admin password, keep signed in, lockout message, help text pointing to where the password lives (Umbrel app Info, or the first-run setup). |
| 2 · Overview | Summary only, each cell linking to its page: the profile waiting for you (with Take control), memory use against the limit, profiles list, browser hours, connect snippet, agents, handoffs this week. |
| 3 · Profiles | Grid of profile cards with a live preview per state (default), or a table. Each profile has an icon: its main site's favicon, read locally from the profile's own favicon cache, or a chosen icon and colour as fallback. Status, mode, agent, memory, activity, auto-stop. ⋯ menu (copy CDP URL, stop, edit, export, delete), stop and delete confirmations, empty state. |
| 4 · New profile | Centred modal over Profiles: name, main site (optional, used for the icon), handoff policy, "start and log in now"; Advanced holds mode, auto-stop, screen size, proxy, local network access and the fallback icon and colour, with a live preview. |
| 5 · Live view | Streamed browser, take or hand back control, controls locked while the agent drives, connection details, agent activity log, help request. |
| 6 · Agents | Agent tokens (create with scope, shown once, revoke), handoff link rules, quick start (curl, Python, MCP, and an Agent tab: a two-line prompt to paste into any agent's chat, pointing it to `/mcp/setup`, with the token kept in `PIER_TOKEN`), endpoint list, MCP. |
| 7 · Settings | Handoff PIN, default handoff policy, memory limit, default auto-stop, default proxy, admin account and sign-out, appearance. |
| 8 · Handoff (phone) | PIN entry, take control on a phone (tap to click, pinch, Type, Fit), note, Done or Can't do this, then expired and finished states. |

### Settings and what they decide

- **Defaults for new profiles** (handoff policy, auto-stop, proxy) are set in Settings and apply to every new profile, including ones an agent creates. An agent can still only raise the policy from the default.
- **Memory limit:** `BROWSERPIER_RAM_LIMIT_PERCENT` wins when it is set, as `BROWSERPIER_ADMIN_PASSWORD` does; otherwise the limit is set in Settings (10–100%, default 70%) and applies at once to the next start.
- **Changing the PIN** needs the current one; changing the password needs the current one and signs out other devices. Wrong tries are limited.
- **Handing back from the app:** when an agent asked for help, the profile page shows the request with **Done, hand back** and **Can't do this** and a note, like the phone page; either answers the agent at once.
- **Agent log:** the profile page lists what agents did in the current session (attached, clicked, typed N characters, took a screenshot, asked for help). It never shows typed text, and it is kept in memory only.
- **Appearance:** light or dark, following the device's own setting unless it chooses System, Light or Dark in Settings. The choice is per device, kept in a cookie, so a phone and a desktop can differ; the sign-in and handoff pages follow it too. Real websites (previews, the live view) keep their own colours, slightly dimmed in dark except while you drive them.
- **Previews:** profile cards show a small screenshot of each running browser. Taking one doesn't count as use, so an open page never keeps a browser from stopping when idle.

### Profile icons

- **Source, in order:** the main site's favicon from the profile's own favicon cache; else the fallback icon and colour chosen in New profile or Edit settings; else a glyph and colour picked from a hash of the profile's name, so it stays the same.
- **Main site:** the one set (`Site`), or, while none is set, the registrable domain (per the Public Suffix List) visited most in the browser's history (`SiteInferred`), recorded at each graceful stop. The inferred one never replaces the one set. A favicon counts for the site when its page is on the site's host or a subdomain of it.
- **Reading:** only after the browser has closed cleanly, at each graceful stop (and when the main site is changed while it's stopped), never while Chromium holds the databases, and never from the network. `chrome/Default/Favicons` and `History` are browser-written, so they're read as hostile: through `os.Root`, as private copies of the main file only (a `-journal` or `-wal` the browser left is ignored, never replayed), opened read-only and immutable, tables checked to be tables, with caps on size (32 and 64 MiB), rows and time (10 s). A failure leaves the cached icon as it was and never fails the stop.
- **The image:** the largest bitmap up to 64 px, else the smallest above, from the icon of most of the site's pages. A PNG, or an ICO holding PNG or 24/32-bit BMP images, of at most 256 KB and 256×256, is decoded and drawn again as a 64×64 PNG; nothing else of the file is kept, and SVG is never taken. It's cached as `profiles/<name>/icon.png`, beside `chrome/`, where the browser can't write.
- **Serving:** `GET /profiles/{name}/icon`, to the admin session only (agent tokens can't), as `image/png` with `nosniff`, `Cache-Control: private, max-age=3600` and an ETag; pages link it with its version.
- **Fallback choices:** glyphs envelope, briefcase, code, document, chat, cart, globe, bank, calendar, cloud, chart, key; colours blue, sky, violet, indigo, teal, cyan, slate. Never orange (a person is needed) or red (failure).

### Export and import

- **Export:** a stopped profile can be exported from Profiles' ⋯ menu, by the admin only (never through the agent API or MCP: an archive is live cookies).
  - **Format:** a gzipped tar encrypted with age and a passphrase of at least 10 characters (`age -d` opens it too). It starts with `manifest.json`: the format version, the BrowserPier version and the profile's settings, main site and fallback icon included, and its cached icon. On import the icon is drawn again like one read from the browser, and one that doesn't pass is dropped; an icon choice this version doesn't know falls back to the default instead of failing the import.
  - **Contents:** the profile's `chrome/` and `files/`, without `run/` (the control socket, and briefly proxy credentials), Chromium's caches and locks, or the mount markers. Only regular files and folders are read, through `os.Root`.
  - **The proxy:** it comes with the archive, password included, since the archive is a secret anyway. So does local network access.
  - **Streaming:** the archive is streamed as it's made, with no temporary file. If anything fails midway, the download is cut off, and an age archive cut short doesn't decrypt.
- **Import:** from Profiles. The archive becomes a new profile under its own name or one you give.
  - **Streaming:** the upload is streamed, never stored whole. It's extracted into a folder of its own, then the profile is created and the folder moved into place last.
  - **What's accepted:** only regular files and folders under `chrome/` and `files/`, through `os.Root`, with caps on total size (8 GiB) and entries. The import stops before it would leave less than 512 MiB of disk free.
  - **Cost cap:** a crafted archive can't make the passphrase check cost more than age's default scrypt work factor (2^18).
- **While a profile is exported or imported** its browser can't start and the profile can't be deleted.

### Interaction rules

- Needs help is visible from anywhere in the UI: an orange status, sorted to the top, and a "N needs you" chip in the shared header.
- Red is reserved for destruction. Stop is neutral; Delete sits in the ⋯ menu and asks for confirmation, naming what will be erased.
- Stop warns when an agent is using the browser.
- Each profile has one main action in a fixed position (Take control, Live view, Start, Retry). Everything else is in the ⋯ menu.
- Take control is blue everywhere; hand back is neutral. Mono type only for things you copy.
- Browser controls are disabled while the agent drives. While the human drives, the keyboard goes to the browser and Esc releases it.
- "Stop browser" is the label everywhere. Copy actions confirm with "Copied".
- Nautical theme in the logo and a few touches of copy; functional labels stay plain.

---

## 10. Anti-bot approach

- Headful Chromium on a virtual display, with no automation flags.
- Visual mode sends real OS-level input through xdotool, not CDP input events. The CDP connection itself is detectable, so in Visual mode no CDP client stays attached to the page.
- Residential IP: a home server's own connection by default, with an optional per-profile proxy. On a VPS, a residential proxy is close to required for protected sites.
- A desktop like a person's: a window manager whose taskbar reserves the bottom of the screen, so `screen.availHeight` is less than `screen.height`, as it is with a panel and unlike headless (PROJ-29).
- Timezone and locale always match the exit IP. Chromium's interface languages are installed, so `Intl` agrees with `navigator.languages`.
- WebRTC sends no UDP, so it can't leak the host IP behind a proxy.
- No fingerprint spoofing: each profile keeps one real, consistent fingerprint over time.
- Persistent, aged profiles with real logins help on their own.
- **Validation** (PROJ-15): `images/chromium/ANTI-BOT.md` has the results against public detection pages, and the signals that remain (SwiftShader, and a 0×0 window as a page's first script often sees it, which a window manager didn't change; its cause is being checked in PROJ-65). `TestVisualModeLooksLikeAPerson` checks Visual mode in CI's Docker tests.
- **Page content** (PROJ-20): Markdown and the main text read only a DOM snapshot, the screenshot comes from the X display, and 20 runs of each against the same detector showed nothing. A PDF is a print: the page sees `beforeprint` and the print media query, as when a person presses ctrl+P, and nothing else. That is recorded in `images/chromium/ANTI-BOT.md` and accepted, since printing is something people do; an agent on a site that watches for it uses Markdown instead.
- **BrowserPier's own CDP use** (the API-mode page tools, PROJ-21): one connection per call, no domain ever enabled, scripts only in an isolated world, nothing written into the page. `TestPageToolsStayQuiet` runs each tool 20 times against a continuous detector (`browsertest.Watch`, on the page's main thread and in a worker), which must catch its positive control, a client doing what Playwright does; the tools showed nothing. The measurements and the residuals (such as `fill` putting its text in at once) are in `images/chromium/ANTI-BOT.md`. Visual mode doesn't use them.

---

## 11. Security

- Private by default: behind Tailscale or a VPN is the recommended setup. Public instances must use HTTPS.
- The orchestrator holds the Docker socket, which is powerful. Keep it minimal, and only ever start containers from BrowserPier's own browser image.
- Raw CDP has no authentication, so it is never exposed directly; it goes through the token-checked proxy.
- CDP access is equivalent to full control of every logged-in account in that profile. Docs say so plainly, and tokens are scoped per profile.
- Tokens, the PIN and the admin password are hashed at rest.
- Agent tokens are never taken in a URL. Clients that can't set headers connect to CDP with a single-use ticket that lasts a minute.
- Treat profile folders as secrets, since they hold live session cookies. With `password-store=basic`, cookies on disk are effectively readable.
- Page text returned to agents is fenced and sanitised (see MCP tools), since agents browse untrusted pages. The page tools' snapshots leave out what is AX-ignored, `display:none`, `visibility:hidden` or zero-size, but keep text a person may not see (transparent, off-screen, clipped or the page's own colour) and accessible names (`aria-label`, `aria-labelledby` even to a hidden node, `alt`, `title`, placeholder): that text is contained, not filtered. On a page too large for the accessibility tree, `find` reads the DOM snapshot's laid-out text alone, which includes `aria-hidden` text (contained the same way). It is fenced and capped like the rest, and the tool descriptions say so. So is the browser container's own output: a session's `last_log`, and Chromium's crash reason after its `error`, since a compromised page can write to it. The UI shows the reason unfenced. The page as Markdown or main text (Page content) follows the same rule, without the AX tree, so `aria-hidden` text a person sees stays; a PDF or a screenshot is the page itself, which no fence can wrap, and a PDF's MCP result says it comes from a web page.
- Pages can't reach the local network, the Docker host or a VPS's metadata endpoint through the browser, unless the admin allows a profile the local network (see Egress).

---

## 12. Deployment and packaging

- **Generic (primary):** `deploy/docker-compose.yml` and `deploy/README.md` (the install guide for a fresh VPS).
  - **The orchestrator container:** runs as the installing user (`PUID`/`PGID`) with the Docker socket's group added, and `./data` bind-mounted (its host path is detected).
  - **Ports:** the app listens on 127.0.0.1:7437 unless `BROWSERPIER_BIND` says otherwise (a Tailscale address, say). Live views use 7438 UDP and TCP.
  - **HTTPS:** the `https` profile adds Caddy for automatic HTTPS, from the shipped `Caddyfile`: no access log, and `?ticket=` and `?token=` redacted from its error log. The guide also covers an existing reverse proxy (WebSockets, long timeouts, no query strings in any log).
  - **Trusted proxies:** the compose network has a fixed subnet (`BROWSERPIER_NET`) with fixed addresses, and only Caddy's is trusted for `X-Forwarded-*`. A proxy on the host is trusted by setting the network's gateway instead.
  - **Image:** `BROWSERPIER_VERSION` defaults to `latest` in the repository, and to the release itself in the files attached to a GitHub release, which is where the install guide downloads them from. `edge` follows `main`.
  - **Releases:** a `v*` tag publishes both images (`0.1.0`, `0.1` and `latest`), then CI makes the GitHub release with the pinned install files and the version's `CHANGELOG.md` section. `RELEASING.md` has the steps.
  - **Configuration:** through environment variables, where an empty value counts as unset. Without `BROWSERPIER_ADMIN_PASSWORD`, the first-run setup page asks for the code printed in the log.
  - **AppArmor:** a one-shot `apparmor` service, behind `--profile apparmor` for Debian 13 and later, loads and installs BrowserPier's AppArmor profile where Docker's may refuse Chromium's sandbox (AppArmor 4 with Docker 29.1 or earlier). BrowserPier waits for it but starts whatever its outcome (see §3, Sandbox).
- **WebRTC on a server:** advertise the public IP (or the Tailscale IP), open one UDP port with a TCP fallback.
  - **TURN:** the `turn` profile adds coturn on the compose network, publishing 3478 UDP and TCP. It takes TURN REST API credentials from `BROWSERPIER_TURN_SECRET` and relays only to BrowserPier's address on that network, over UDP only: every other peer is denied (the host, the LAN, other containers, the internet), so it is no open relay, and its relay ports stay unpublished. An allowed address covers all its ports, so refusing TCP relays (`no-tcp-relay`) is what keeps the web app out of reach. Each viewer may hold 40 relays, 300 in all: generous, since a page takes one per TURN URL and network interface, and a UDP relay a page left without releasing counts until it times out. Its settings go to a file, not its command line, so the secret doesn't show in the host's process list. It runs with no capabilities but `NET_BIND_SERVICE` (its binary carries it as a file capability and won't start without it), no-new-privileges and a read-only root, and answers no unauthenticated STUN (`no-stun`).
  - **TLS on 443 isn't bundled:** Caddy holds 443. TURN over TCP on 3478 covers networks that only block UDP; for those that allow only HTTPS, the guide points to a TURN server elsewhere (`turns:` on 443), set with the same variables.
- **Umbrel:** `yakitori-browserpier` in the author's community app store (`Oni-giri/yakitoris-app-store`); the source is `umbrel/yakitori-browserpier/`.
  - **Sign-in:** the app password (`deterministicPassword`) is the admin password. Umbrel's login stays on for the UI; `/api`, `/cdp`, `/mcp`, `/h` (handoff links) and `/static` are whitelisted, since agents and phones can't sign in to Umbrel and BrowserPier checks them itself.
  - **Ports:** Umbrel opens it on **7436** (7437 is taken by another app in Umbrel's store); BrowserPier still listens on 7437 inside. Live views publish 7438 UDP and TCP directly.
  - **No TURN:** an Umbrel is reached on the LAN or Tailscale, where live views connect directly, and a TURN server at home would need ports forwarded on the router.
  - **Live-view addresses:** `exports.sh` reads the host's own addresses (LAN and Tailscale, not Docker's bridges) when the app starts, since the container can't see them.
  - **Trusted proxies:** only the app network's gateway, which Umbrel's proxy (part of umbreld since umbrelOS 2.0) connects from. Other apps on that network can't forge `X-Forwarded-*`. The app therefore requires umbrelOS 2.0 (`manifestVersion: 2.0`); older versions, whose proxy was a container, refuse to install it.
  - **Stopping:** with `BROWSERPIER_STOP_BROWSERS_ON_EXIT=true`, BrowserPier stops its browsers on the way out, through the normal stop, once it accepts no more requests. So stopping, updating or uninstalling the app in Umbrel ends its sessions (logins are kept). This differs from the generic install, where browsers keep running through an update. A `post-stop` hook stops any browser left over.
  - **AppArmor:** a `pre-start` hook loads BrowserPier's AppArmor profile at every start of the app, after a reboot too (see §3, Sandbox). Umbrel's app update copies only the files it knows (`docker-compose.yml`, `umbrel-app.yml`, `exports.sh`, `hooks/`, `icon.svg`), not extra folders, so the hook carries the profile and its loader as heredocs, generated from `images/chromium/apparmor/` and kept identical by a test. It always exits 0, since Umbrel ignores a hook's exit status, and says in one line of umbreld's journal whether it loaded the profile, didn't need to, or couldn't, and why. A failed load leaves browsers under Docker's profile if the sandbox works there, otherwise unconfined, as BrowserPier's log says.
  - The Docker socket mount keeps it out of Umbrel's official store, whose rules forbid it. Being in a community store, it isn't subject to that store's review of hooks either.
- Browser containers are started by the orchestrator, not listed in the compose file. (The `apparmor` service runs the browser image too, but only its profile loader.)
- App data holds the database and one folder per profile.
- The orchestrator listens on port **7437** by default ("PIER" on a phone keypad), set with `BROWSERPIER_LISTEN`. It avoids 8080 and other ports common among self-hosted apps. (On Umbrel, where app ports must be unique, it's opened on 7436: see above.)
- **Profile export and import:** from a profile's ⋯ menu, download it as a password-encrypted archive, and import one into a new profile. A profile must be stopped to export, so its files are consistent.

---

## 13. Decisions

| Question | Decision | Why |
|---|---|---|
| Live stream | WebRTC (pion), H.264 from the virtual display | Smooth, and no CDP attached to the page, so Visual mode stays hard to detect. |
| Orchestrator stack | Go, single binary, SQLite | Low idle RAM, native Docker SDK and pion. |
| Frontend | Go templates + htmx, small vanilla JS for WebRTC | No build step; everything ships in the one binary. |
| Notifications | None from BrowserPier | The agent already messages the human with the handoff link. |
| RAM limit | Stop the longest-idle unused browser, else refuse; the same when the host is short of free memory, with no extra setting | Agents get a browser when possible and a clear error when not, instead of hanging in a queue. A host short because of other apps would otherwise swap, or have its OOM killer end another app or a browser mid-task. |
| Stop errors | A stop Docker reports as failed counts when the container has stopped, with a warning in the log | The browser closed the usual way, and its exit status still tells a clean close from a crash. Refusing answered 502 and could record the session as crashed, over a fault in the host's network teardown that only the host can fix. |
| Profile backups | Encrypted export and import in the UI | Losing the server shouldn't mean logging in everywhere again. |
| Deployment | Generic docker-compose first (any VPS), Umbrel app second | The author runs it on Umbrel, but it should work for anyone on a plain server. |
| Fingerprints | No spoofing; one stable real fingerprint per profile | Spoofing needs CDP on every page, and a changing fingerprint flags logged-in accounts. |
| Proxies | Per-profile, authenticated, via a forwarding proxy; timezone and locale matched | On a VPS, residential proxies are what makes protected sites usable. |
| Page egress | Through the forwarder in the container, which refuses private and link-local addresses; a profile can be allowed the local network | The browser shouldn't be a way into the LAN, the host's other apps or a VPS's credentials. Kept in the container for now: `--network none` makes Chromium report itself offline. |
| CDP without headers | Single-use, one-minute tickets; the token is never taken in the URL | A proxy in front may log URLs, and a logged token would be good until revoked. `?token=` was removed before the first release rather than deprecated, as nothing depended on it. |
| Orchestrator ↔ browser container | A Unix socket in a per-profile folder shared with the orchestrator | Nothing listens on a network, so a page or another container can't reach CDP. |
| Profile storage | Bind-mounted folders in the data dir, not named Docker volumes | One directory to back up or move; export and downloads read files directly. The host path is detected from the orchestrator's own mounts. |
| License | Apache 2.0, not AGPL | Self-hosters are the users, and many companies forbid AGPL code, even for internal tools. Same license as Steel; the patent grant and NOTICE come with it. |
| Chromium sandbox | On, with a seccomp profile that allows user namespaces | Pages are untrusted; the container is a second boundary, not the only one. |
| Desktop in the browser container | JWM with a 32-pixel taskbar along the bottom (a clock only); the browser window at 0,0, unframed, fills the rest | A bare Xvfb screen has no work area, which detection reads as headless. A real window manager reserves the taskbar's strip, so nothing is faked. At the bottom, the window stays at 0,0 and Visual-mode coordinates stay screen coordinates. About 7 MB, with no terminal emulator: an empty package stands in for the one jwm depends on. |
| TURN credentials | Each viewer's own, derived from a shared secret (TURN REST API) and expiring, with one stable user id per viewer; a static login only for services without it | A static password reaches every viewer's browser and works for anyone it leaks to until changed. Derived ones run out for new relays, and the secret stays on the server. A stable id per viewer lets the TURN server's per-user quota stop one handoff holder from taking every relay. |
| Bundled TURN | coturn on the compose network, relaying only to BrowserPier's address there | A TURN server that relays anywhere is a way into the LAN and a free relay for strangers. Relaying over the compose network also needs no relay port range published and no hairpin through the host's public address. |
| Page to Markdown and main text | Rendered in Go from a DOM snapshot (`internal/pagetext`), not with html-to-markdown or go-readability | The libraries read HTML and can't see computed styles, so they would keep hidden text; each brings half a dozen modules for a few hundred lines; and the DOM snapshot is already read, measured and quiet. |
| Screenshot in API mode | From the X display, as in Visual mode | No CDP at all, so nothing for a page to notice, and the same picture in both modes. |
| AppArmor for browser containers | Docker's, except on AppArmor 4 hosts without Ubuntu's userns restriction (Debian 13, umbrelOS 2.0): there, BrowserPier's own `browserpier` profile (Docker's plus `userns`), loaded by an opt-in compose service or Umbrel's pre-start hook, and checked; Docker's when it works there (Docker 29.9 and later); none, with a warning, if neither | Docker 29.1 and earlier's profile refuses the sandbox there. Losing that backup layer costs less than losing Chromium's sandbox, and less than refusing to start browsers on servers updated without the loader. |

### Still open

- **Logo:** navy style (v1.1).

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
- Web UI (8 screens, see `design/`), encrypted profile export/import
- Generic docker-compose with HTTPS (Caddy), first-run setup, Umbrel app
- Anti-bot validation
- TURN support for viewers on UDP-blocking networks (planned for v1.1, done early): bundled coturn or your own

### v1.1: efficiency and reach

- Content endpoints: page to Markdown or readability text, screenshot, PDF (done, PROJ-20: see Page content)
- Token-efficient API-mode tools: accessibility snapshot with stable refs, find, batch, covered-element check (done, PROJ-21: see API-mode page tools)
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
