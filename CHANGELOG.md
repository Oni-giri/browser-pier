# Changelog

## 0.6.0

**Before you update:**
- The browser definition changed (file choosers without a dialog, Visual mode's element tree, exact typing, and fencing a person's control as it ends), so browsers started by 0.5.x are closed gracefully on the first start of 0.6.0, logins kept, and start again under the new definition the next time they're used. If one is still running afterwards, stop it: until it restarts it keeps Chromium's old file dialog, which can hand a page any file in its container.
- **0.6.0 is published for amd64 only,** like 0.5.0. On arm64 (a Raspberry Pi, say), stay on 0.4.0 until a later release brings it back.
- **Sites see a new WebGL renderer, once:** WebGL now runs on Mesa's llvmpipe instead of SwiftShader, so each profile's WebGL and canvas fingerprint changes with this update, then stays stable.
- **`BROWSERPIER_USERNS_MODE`** takes only `keep-id` (or `keep-id:uid=…,gid=…`), or nothing; BrowserPier refuses to start with any other value, `host` included.
- **No going back to 0.5.x:** 0.6.0 adds to the database, and 0.5.x won't open it afterwards. Back up `./data` first.

**Changed for people running BrowserPier**
- **Two-step sign-in,** off by default: Settings → Admin account → Two-step sign-in asks for an authenticator app's code after the password, with ten recovery codes shown once. Full sign-in handoffs ask for it too; agent tokens and other handoffs never do. Lost the phone and the codes: `browserpier 2fa reset` on the server (`BROWSERPIER_ADMIN_PASSWORD` resets only the password). Worth turning on for a public server.
- **A webhook when a person is needed** (Settings → Notifications), for agents with no chat to pass a handoff link through. One signed POST on `handoff.requested`, `handoff.expired`, `handoff.cancelled` and, if ticked, `browser.failed`, with a sentence Slack and Discord show as it is. It holds BrowserPier's own words, never the agent's text or the page's, and the handoff link only for Link + PIN and Full sign-in (a chat's link preview could open a Link only one). Links are built on `BROWSERPIER_PUBLIC_URL`, or **Address for links** (set it on Umbrel). See "Notifications" in `deploy/README.md`.
- **The activity log outlives the browser.** A stopped profile says how its last run ended ("Stopped: idle for 30 min"), and a log page per profile keeps the last 5 runs for 24 hours, in memory: agents' actions, control, watching, handoffs and tab titles.
- **Proxies:** write `{sticky}` where your provider takes a session code, and each profile fills in its own, so one default proxy gives each profile its own exit; **New exit IP** in its ⋯ menu picks another. **Test**, next to every proxy field, shows the exit IP, country, time zone and languages a browser would get, before any browser uses it.
- **File choosers in the live view and on the handoff page:** while you have control, a page's file chooser opens a box with the profile's uploads, **Upload from this device** and Cancel, instead of a dialog in the browser.
- **Handing back takes effect before it says so:** the agent gets the browser back only once it has refused whatever of your control was still on its way. Meanwhile the handoff page says **Handing back…**.
- **The live view's Type** types accents, other alphabets and emoji exactly, and Hand back stops a Type under way.
- **The browser:** Back loads the page again rather than keeping old pages in memory for minutes; the address bar makes no search suggestions, so a typed address goes nowhere before Return; WebGL draws at about half its former frame rate, its GPU process sandboxed again (see Fixed); and Visual-mode browsers run with Chromium's accessibility on, which no page API reports but a page inserting thousands of elements at once could time.
- **Every container is checked before it's created** against a fixed list of what it may get (no published port, device, host folder or other image). A refusal fails the start, naming the setting: please report it.
- **Orange only means someone is needed:** a missing PIN and the export passphrase's caution are grey now.

**Changed for agents**
- **A handoff ends only once the browser is back:** `GET /api/handoffs/{id}` and `wait_for_handoff` read `pending` until the agent can act again. A withdrawal waits up to 30 s for that, then answers 503. While BrowserPier shuts down, requests answer 503. `request_handoff` says `notified: true` when the admin's webhook was told.
- **Visual-mode typing:** control characters other than `\n`, `\r` and `\t`, and U+FFFD, are refused at once (400), and a failure says whether anything may have been typed. Over MCP, `type_text` refuses text it estimates at over 45 s (about 800 CJK characters at instant pace), so it isn't retried and typed twice: split it.
- **Keys after a shortcut wait:** a `key` or `type` less than 150 ms after a ctrl, alt or super combination or an F-key waits out the rest, so text after `ctrl+l` reaches the address bar. A slow page can still take it: open addresses with `open_url`.
- **Revoking a token** breaks off its Visual-mode input, which answers 401.
- **`snapshot`:** a line of page text that starts like one of BrowserPier's (a ref, `[`, `-- `, `URL:`, `Title:`) now starts `text: `.
- **Many tabs:** from 10 open tabs on, the page tools add a line with the count.

**New**
- **Call ids,** so a retried action isn't run twice: `Idempotency-Key` on `/click`, `/type`, `/computer`, `/act`, `/navigate` and `POST /tabs`, or `call_id` on the matching MCP tools. A repeat answers how the first went, with `Idempotency-Replayed: true`. `call_status` and `session_status` (REST `GET /api/sessions/{id}/calls/{call_id}` and `GET /api/sessions/{id}`) say how calls went. Other routes refuse the header, and action bodies with anything after the JSON are refused.
- **`visual_snapshot`:** in Visual mode, the page's controls and text with refs and positions, read from Chromium's accessibility with no CDP attached; secret fields show only `[filled]` or `[empty]`. `click` takes a `ref` from it, and answers 409 if the element is gone, off screen or covered.
- **`open_url`** (`/computer` `open`): opens an address by clicking the address bar, so a busy page can't take it; refused (409) when a pop-up, menu or full screen is in the way.
- **`choose_files`** (`/api/sessions/{id}/chooser`): answers a page's file chooser with files from the uploads, or cancels it. `list_files` and action notices (`X-File-Chooser` over REST) show open choosers.

**Fixed**
- **A page could be handed any file in the browser's container,** the cookie database included, through Chromium's file dialog in Visual mode. The dialog is gone; choosers take only the profile's uploads.
- **A live view whose control had ended elsewhere** (a handoff ending, Hand back in another tab) could still send input, read or overwrite the clipboard, or end the takeover of whoever took control next. Control now belongs to the takeover that took it.
- **A Take control that lost to a stop** could leave agents locked out.
- **Chromium's GPU process ran without its own sandbox.** It's sandboxed now, and if it can't be, pages get no WebGL.
- **Visual-mode typing** of accents, other alphabets and emoji failed or dropped characters.
- **The reason a browser ended** could name an older line from a child process instead of the browser's own.
- `browserpier token list` showed tokens made in the same millisecond in random order.

## 0.5.0

**Before you update:**
- The browser definition changed (the browser image now has a recording pipeline), so browsers started by 0.4.x are closed gracefully on the first start of 0.5.0, logins kept, and start again under the new definition the next time they're used.
- **0.5.0 is published for amd64 only.** There is no arm64 image this time; stay on 0.4.0 on an arm64 machine (a Raspberry Pi, say) until a later release brings it back.

**Changed for people running BrowserPier**
- **Session recording.** A profile with **Record sessions** on keeps what agents do on its screen, for you to replay. It is off by default, for new and existing profiles, and only the admin turns it on, in the profile's settings; it applies at once, to a running browser too.
  - **What's recorded:** only while an agent acts, and for 30 s after, at 5 frames a second and 3/4 of the screen size; a still screen records almost nothing.
  - **Never a person:** recording stops before Take control or a handoff lets a person's input reach the page, and starts again only once the agent has the browser back and its clipboard has been emptied. The live view's hint says so, in full screen too.
  - **Where to watch:** the profile page shows **Recording** or **Recording paused**, and a **Recordings** cell with one row per session. The player has speeds from 1× to 8×, a timeline that shows the gaps and why (a person had control, the screen was still, the browser was idle…), Skip gaps, and links from the agent's activity log to the moment.
  - **Room:** Settings sets how long recordings are kept (1 to 90 days, 7 by default) and how much space they take in all (2 GB by default). 1 GB of the disk is always left free, and the profile holding the most gives up its oldest recording first.
  - **Privacy:** recordings show what a screenshot would. They live in `data/recordings/`, outside the profile folders, and aren't encrypted apart from the data dir; leave `data/recordings` out of your backups if you don't want them there. Deleting a profile deletes its recordings.
  - **Cost:** about 3–4% of one CPU core and 33 MB of memory in each browser while it records.

**Changed for agents**
- Nothing new to call: agents can't read, list, start, stop or turn off a recording, so an agent can't hide what it did.

**Fixed**
- A hand-back that happens while a browser is stopping, where the stop then fails, no longer leaves the person's text on the browser's clipboard: it is emptied before the browser is back in service.

## 0.4.0

**Before you update:** the browser definition changed (the browser image now has a clipboard tool), so browsers started by 0.3.x are closed gracefully on the first start of 0.4.0, logins kept, and start again under the new definition the next time they're used.

**Changed for people running BrowserPier**
- **Copy and paste in the live view.** Ctrl/⌘+C, X and V now work inside the remote browser, and on a Mac ⌘ shortcuts (⌘A, ⌘C, ⌘V, ⌘Z…) act as Ctrl. To move text between your computer and the browser, use the new **Clipboard** button in the live view's bar:
  - Opening the box (or Refresh) shows what the browser has copied, and **Copy** puts it on your clipboard.
  - Paste your own text into the box, then **Send to browser** to put it on the browser's clipboard for the rest of your takeover, or **Paste into page** to paste it straight in.
  - The box is text only, up to 10,000 characters sent at once. The browser's clipboard is read only when you open or refresh the box, never in the background.
- **The browser's clipboard is emptied when you hand back,** however the takeover ends (Hand back, a handoff's Done or expiry, closing or reloading the page), so the agent can't paste what you copied or sent, by keyboard or middle-click.
- **The status pill moved into the live view's bar,** so it no longer covers the browser's tabs and address bar.
- **Full screen** for the live view on the profile page and the handoff page, with Take control / Hand back (and the handoff's Done and Can't do this) still at hand. Esc leaves full screen and releases the keyboard. On iPhone, where the browser has no full screen, the view fills the window instead. The remote browser's size never changes.
- **On a phone, Type** types its text again (the clipboard box covers pasting), so fields that refuse pasting can still be filled.
- A live view that can't connect says why and to reload, instead of waiting with no picture.

**Changed for agents**
- After a person's takeover ends, the browser's clipboard and its selection are empty, before the agent gets the browser back.
- The activity log says "Clipboard set" or "Pasted from the clipboard box" when the person used the box; it never shows the text or its length.

## 0.3.0

**Before you update:** the browser definition changed, so browsers started by 0.2.x are closed gracefully on the first start of 0.3.0, logins kept, and start again under the new definition the next time they're used.

**Changed for agents**
- **Input moves at a hand's pace by default** (`pace: human`), on `/computer`, `click`, `type_text`, `press_keys`, `scroll`, `drag` and act's steps. The pointer glides there in 0.2–1.15 s, lands a few pixels from the middle of the target (within about 3 px in Visual mode), pauses about 0.16 s and holds the button about 0.12 s; typing takes about 0.25 s a character, with a typist's uneven gaps. `pace: instant` is input as it was before. Expect actions, and typing above all, to take longer.
- **Typing caps:** at human pace, `type` takes at most 500 characters a call over REST (it was 2,000; 2,000 still at instant pace in Visual mode), and act's human-paced type steps 500 together. Over MCP (`type_text`, and act's human-paced type steps together) the cap is 150 characters, about 40 s, so the call ends before an MCP client's request timeout and isn't retried. Split longer text across calls, use `fill`, or set `pace: instant`.
- **One input at a time per browser:** a Visual-mode input sent while another is still running answers 409 ("another input is still running"). A person's **Take control** in the live view now breaks off an input action, or an `act` call, that has run 3 s or more, instead of being refused for its whole length: the call answers 409 (act's says which step a person took control in and how many steps ran), and the activity log says it was interrupted.
- **`wait_for` says how it went:** `already true when wait started: …` when the condition held at the first look (the wait proved nothing), `waited 1.4 s for …`, or `timed out after 10 s waiting for …; now: …` with the state it found. A ref whose page navigated away ends the wait at once (`gave up after …`). Anything parsing the old answers needs updating.
- **act reports what changed:** once the agent has a snapshot of the page, act's answer ends with `### Changes`, what changed on the page since that snapshot, instead of nothing. Pass `changes: false` to leave it out.
- **`generic "…"` lines in snapshots:** controls built from a div or a span (no role, only a pointer cursor or a tabindex) now appear as `generic "<text>"` with a ref, so they can be clicked.
- **Back, forward or reload onto the browser's error page answers 502,** with the browser's error, as `navigate` to a failing address already did.
- **Tabs come back after a stop:** a browser starts again with the tabs it had, the one in front still in front, and keeps session-only cookies, so a stop or the idle auto-stop no longer logs out of sites whose login is a session cookie ("remember me" unticked, many SSO and bank sessions). The exception is a session BrowserPier typed a secret into: the next start opens none of its tabs, logins still kept.

**Changed for people running BrowserPier**
- **New profiles block ads and trackers** by default (Settings → default for new profiles; per profile under Advanced). Existing profiles keep running as before, with blocking off. To fill the list, BrowserPier downloads two public lists (Peter Lowe's ad servers and EasyPrivacy) into `<data dir>/blocklists/` and checks them every 4 days. Set `BROWSERPIER_BLOCKLIST_URLS=none` to download nothing, or point it at lists of your own. Blocking is by host name in the browser container's network forwarder: pages see a listed host's requests fail, as they do with any DNS-level blocker. Challenge and CAPTCHA providers are never blocked.

**New**
- **Human-like input:** `pace` on every input action, with the glide, aim, hold and typing rhythm above.
- **`snapshot` diff and act's changes:** `snapshot` with `diff: true` gives only what changed since the last one.
- **Covered clicks wait:** act aims at a wrapped link's largest box, and when something covers an element it scrolls it to the middle and waits up to `cover_wait_ms` (1 s by default) for the cover to go.
- **Download notices:** act ends with `### Downloads`, MCP `click` and `press_keys` say what they downloaded, and REST `/click` and `/computer` add an `X-Downloads` header; `download_wait_ms` waits for a download to finish.
- **Uploads:** MCP `upload_file` puts a file (up to 20 MiB) in the session, and act's `upload` step sets it on a file input.
- **Handoffs that hand back by themselves:** `request_handoff` takes `until_url`, and the handoff ends as done once the person's tab in front reaches a matching address. The person's page says so.
- **Ad and tracker blocking,** per profile, with `block_ads: false` (REST) or `allow_ads` (MCP `start_browser`) to turn it off for one session.
- **Cloudflare challenges:** the page tools spot Cloudflare's challenge page and say where its checkbox is on the screen, for a real click (`/computer`, or MCP `click`, which now works on API-mode sessions) or a handoff. BrowserPier never clicks it by itself.
- **`navigate` waits out script redirect chains,** such as a sign-in bounce, and names the address that failed after a redirect.
- **`wait_for` takes `load`,** for an app that loads, then fetches its content: it waits until the page and its requests have been quiet for 500 ms.
- **Targets:** act steps and `wait_for` can name an element by `role=button[name="Save"]` or `text=…` instead of a ref, with no snapshot first.
- **Div and span controls** are listed and clickable, and `role=generic` targets find them.
- **Filled secrets are hidden:** a value BrowserPier types itself is replaced by `<hidden>`, in every common encoding, in every answer to agents and in the activity log. This is the groundwork for vault fills; nothing registers a secret yet.

**Fixed**
- A browser with an input action under way is no longer stopped as idle in the middle of it.
- An element that goes with its page during a call is reported as the page changing (409, take a new snapshot), not as "can't reach the browser" (502).
- `navigate` to a download address, or one that answers an empty 204, answers at once that the tab stayed where it was, instead of waiting for an error page.
- A `text=` target never matches the value inside a field, whatever role the page gives the field.
- Upload names with two dots inside them (`report..v2.pdf`) are accepted wherever the REST upload accepts them.

## 0.2.1

**Fixed**
- **AppArmor on Umbrel:** BrowserPier's own AppArmor profile now loads. Umbrel's app update copied the start hook but not the files it loaded, so on Umbrel browsers kept Docker's profile instead. That still confines them where Chromium's sandbox works under it, as on umbrelOS today. The hook now carries the profile itself and says in one line what it did. When BrowserPier falls back to Docker's profile or none, its log now says why its own was skipped.
- **Copy buttons over plain HTTP,** as on Umbrel: they used to do nothing, the token's Copy button included. They now copy, or say they couldn't. One click on the token selects it whole.
- **`<!--` in page text no longer hides the rest of the answer.** Page text containing `<!--` (a code sample, a forum post) used to drop everything after it from `snapshot`, `find` and the other page reads. It is now kept, written `<\!--`.
- **Odd iframe addresses and navigation names stay on their own line** in `snapshot` and `find`, so a page can't make them look like separate lines.

**Changed for agents**
- **A control under an iframe:** when `act` can't click one because an iframe covers it, often a cookie or consent banner from another site, the error names the frame's real origin. It also points to the ways past: `request_handoff` for a person, or Visual mode. `snapshot` and `find` mark such controls `[under an iframe]`.
- **Redirects:** `navigate` says `Redirected from: …` when the page ended up at another address.
- **New tabs:** the new-tab notice shows the tab's path, not just its site. The query is left out; `tabs` shows the full address.

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
