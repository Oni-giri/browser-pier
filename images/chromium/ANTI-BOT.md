# Anti-bot validation

How the browser looks to bot detection, in both modes. Checked for PROJ-15 on 2026-10-30, with Chromium 156 on Debian trixie, from an OVH server (a datacenter IP) with the default exit-location matching on (France: `Europe/Paris`, `fr-FR`). See SPEC.md §10 for the approach.

## Automated check

`TestVisualModeLooksLikeAPerson` (`internal/api/antibot_docker_test.go`, `make test-docker`) opens a detection page (`internal/browsertest/detect.go`) in a real browser container. The page reports what detection scripts read back to the test, so Visual mode is checked without CDP.

- **Visual mode:** the page is opened as a person would, through the address bar with xdotool. The test checks:
  - no `navigator.webdriver` and no automation globals
  - a normal user agent, plugins and `window.chrome`
  - a real window size, and a page that has focus
  - a taskbar: `screen.availHeight` under `screen.height`, and the window filling the rest
  - languages, `Intl` and the time zone that match the exit location
  - from the browser's side, **no CDP client attached to the page** (`Target.getTargets` over a browser-level connection, which attaches to nothing)
- **API mode:** the control. A CDP client that enables `Runtime`, as Playwright and Puppeteer do, shows as attached.

## Public test pages

Visual mode was driven only through the address bar, clicks and screenshots. API mode was driven by Playwright over BrowserPier's CDP endpoint, as an agent would.

| Page | Visual mode | API mode |
|---|---|---|
| [bot.sannysoft.com](https://bot.sannysoft.com/) | All pass except **WebGL renderer: SwiftShader** | Same |
| [BrowserScan bot detection](https://www.browserscan.net/bot-detection) | **Normal** (webdriver, user agent, CDP, navigator) | **Normal**: its CDP check doesn't catch Playwright on Chromium 156 |
| [deviceandbrowserinfo: Are you a bot?](https://deviceandbrowserinfo.com/are_you_a_bot) | **"You are human!"**, every signal false | **"You are a bot!"**: `isAutomatedWithCDP` (main thread and workers) |
| [CreepJS](https://abrahamjuliot.github.io/creepjs/) | 0% headless, 0% stealth, 50% "like headless"; 44% since PROJ-29's taskbar | Same |
| [nowsecure.nl](https://nowsecure.nl/), [scrapingcourse.com Cloudflare challenge](https://www.scrapingcourse.com/cloudflare-challenge) | Cloudflare's interactive checkbox | Same |

**How to read these:**
- **CDP is the big difference between the modes**, as SPEC §10 expects. Pages that look for a CDP client find one in API mode and nothing in Visual mode. Use Visual mode for protected sites.
- **Cloudflare challenged both modes** from this datacenter IP. Clicking through it automatically wasn't attempted: that's what handoffs are for. A person finishes it from the handoff link, and the profile keeps the clearance cookie. From a residential connection (a home server, or a residential proxy), fewer challenges are expected.
- **With location matching off,** the browser ran in UTC with English, and deviceandbrowserinfo flagged that combination as a weak bot signal, even in Visual mode. Keep `BROWSERPIER_GEOIP_URL` on, or set a profile's time zone and language by hand.

**Rechecked after PROJ-26,** which sends every connection through the forwarder in the container (no QUIC, and WebRTC over TCP only): Visual mode is still "You are human!" on deviceandbrowserinfo, and sannysoft is unchanged.

## Signals that remain

None of these is spoofed, by design (SPEC §10). They come from the container:

- **SwiftShader WebGL renderer.** There's no GPU in the container, so WebGL is software-rendered and says so. It's the one red line on sannysoft, and a known server signal. Giving containers the host's GPU would fix it where there is one (PROJ-30).
- **A 0×0 window, often, as a page starts.** In both modes, a page's first inline script often sees `outerWidth` and `outerHeight` as 0, which headless browsers report. It reads normally once the page has loaded. A window manager didn't change it (PROJ-29, below). The likely cause, from reading Chromium's source and not verified: a new document's widget hears its window's bounds from the browser only once it's shown, after the commit, while the renderer may already be running the page's first script (`RenderWidgetHostImpl::SendScreenRects` skips hidden widgets). If so, desktop Chrome does the same. PROJ-65 checks both.
- **No speech voices** (`speechSynthesis.getVoices()` is empty), as on many Linux desktops.

## API mode, specifically

- **Focus:** a page opened with CDP's `Page.navigate` leaves the keyboard focus in the address bar, so `document.hasFocus()` is false until something clicks the page. Visual mode, typing a URL and pressing Enter, gives the page focus as a person's browser does.
- **CDP detection:** `isAutomatedWithCDP` and similar checks see the client (see above).

## A desktop with a taskbar (PROJ-29)

Xvfb ran no window manager or panel, so `screen.availHeight` equalled `screen.height`, one of CreepJS's "like headless" hints. The image now runs JWM (Debian's `jwm`, plus `xterm`, which it depends on; about 8 MB in all) with a 32-pixel taskbar along the bottom, which reserves its strip of the screen through `_NET_WORKAREA` as a person's panel does. Nothing is spoofed: Chromium reads the work area from the window manager. The browser window sits at 0,0 with no frame and fills the screen above the taskbar, so Visual-mode coordinates are still screen coordinates, and the taskbar shows in screenshots and the live view. The taskbar shows only a clock, in the exit's time zone; there are no menus, key bindings or window buttons, so nothing on the desktop starts a program or kills Chromium, and the window can't be moved or resized from the desktop. Fullscreen covers the taskbar, as elsewhere.

Measured on Chromium 156 with `TestMeasureWindowAtLoad` (`go test -tags 'docker measure' -run TestMeasureWindowAtLoad -v ./images/chromium/`, a 1280×800 screen), 110 loads of the detection page each, before and after:

| | Before (no window manager) | After (JWM and taskbar) |
|---|---|---|
| `screen.availHeight` | 800 in 110 of 110 loads | **768** in 110 of 110 |
| Window once loaded | 1279×799 (a pixel short of the screen) | 1280×768, the work area exactly |
| 0×0 at the first script: Visual, fresh containers | 14 of 20 | 14 of 20 |
| … Visual, new tabs | 20 of 30 | 20 of 30 |
| … Visual, the same tab again | 24 of 30 | 28 of 30 |
| … API mode, `Page.navigate` | 16 of 30 | 18 of 30 |

So the taskbar hint is gone, and the 0×0 at load is unchanged (see Signals that remain). On CreepJS, in Visual mode on this machine, "like headless" went from 50% to 44%: `noTaskbar` is now false. Of what it still counts, SwiftShader is the container's; the rest (`noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax`: APIs desktop Linux Chromium doesn't have, and the default light theme) aren't about the container. The rate depends on timing: these runs, on 2026-11-12, shared 6 CPUs with other work, and PROJ-15 saw about a third.

## Fixed along the way

- **`Intl` contradicted the browser's languages.** The image had only Chromium's `en-US` interface, so whatever `LANGUAGE` said, `Intl` reported `en-US` while `navigator.languages` and Accept-Language followed the exit location (`fr-FR`). The image now installs `chromium-l10n` (about 120 MB), and its build fails if the locales are missing. `Intl` now follows the interface language: `de` for a German exit, as Chromium on a German Linux desktop reports.
