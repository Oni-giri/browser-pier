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
| [bot.sannysoft.com](https://bot.sannysoft.com/) | All pass except **WebGL renderer: SwiftShader**; all pass since WebGL moved to llvmpipe (PROJ-148: measured from a residential connection, not this OVH address; see below) | Same |
| [BrowserScan bot detection](https://www.browserscan.net/bot-detection) | **Normal** (webdriver, user agent, CDP, navigator) | **Normal**: its CDP check doesn't catch Playwright on Chromium 156 |
| [deviceandbrowserinfo: Are you a bot?](https://deviceandbrowserinfo.com/are_you_a_bot) | **"You are human!"**, every signal false | **"You are a bot!"**: `isAutomatedWithCDP` (main thread and workers) |
| [CreepJS](https://abrahamjuliot.github.io/creepjs/) | 0% headless, 0% stealth, 50% "like headless"; 44% since PROJ-29's taskbar, 38% since PROJ-148's llvmpipe (residential connection, not this OVH address; see below) | Same |
| [nowsecure.nl](https://nowsecure.nl/), [scrapingcourse.com Cloudflare challenge](https://www.scrapingcourse.com/cloudflare-challenge) | Cloudflare's interactive checkbox | Same |

**How to read these:**
- **CDP is the big difference between the modes**, as SPEC §10 expects. Pages that look for a CDP client find one in API mode and nothing in Visual mode. Use Visual mode for protected sites.
- **Cloudflare challenged both modes** from this datacenter IP. Clicking through it automatically wasn't attempted: that's what handoffs are for. A person finishes it from the handoff link, and the profile keeps the clearance cookie. From a residential connection (a home server, or a residential proxy), fewer challenges are expected.
- **With location matching off,** the browser ran in UTC with English, and deviceandbrowserinfo flagged that combination as a weak bot signal, even in Visual mode. Keep `BROWSERPIER_GEOIP_URL` on, or set a profile's time zone and language by hand.

**Rechecked after PROJ-26,** which sends every connection through the forwarder in the container (no QUIC, and WebRTC over TCP only): Visual mode is still "You are human!" on deviceandbrowserinfo, and sannysoft is unchanged.

## Signals that remain

None of these is spoofed, by design (SPEC §10). They come from the container:

- **Software WebGL renderer.** There's no GPU in the container, so WebGL is software-rendered and says so: `ANGLE (Mesa, llvmpipe (LLVM 19.1.7 256 bits), OpenGL 4.5)` since PROJ-148, which is what a Linux desktop or VM without a GPU driver reports. That is no longer SwiftShader, which was the one red line on sannysoft and an item of CreepJS's "like headless", but a detector can still read llvmpipe as "a VM, or no GPU". Giving containers the host's GPU would replace it where there is one (PROJ-30).
- **A 0×0 window, often, as a page starts.** In both modes, a page's first inline script often sees `outerWidth` and `outerHeight` as 0, which headless browsers report. It reads normally once the page has loaded. A window manager didn't change it (PROJ-29, below). The likely cause, from reading Chromium's source and not verified: a new document's widget hears its window's bounds from the browser only once it's shown, after the commit, while the renderer may already be running the page's first script (`RenderWidgetHostImpl::SendScreenRects` skips hidden widgets). If so, desktop Chrome does the same. PROJ-65 checks both.
- **No speech voices** (`speechSynthesis.getVoices()` is empty), as on many Linux desktops.

## API mode, specifically

- **Focus:** a page opened with CDP's `Page.navigate` leaves the keyboard focus in the address bar, so `document.hasFocus()` is false until something clicks the page. Visual mode, typing a URL and pressing Enter, gives the page focus as a person's browser does.
- **CDP detection:** `isAutomatedWithCDP` and similar checks see the client (see above).

## A desktop with a taskbar (PROJ-29)

Xvfb ran no window manager or panel, so `screen.availHeight` equalled `screen.height`, one of CreepJS's "like headless" hints. The image now runs JWM (Debian's `jwm`, about 7 MB; an empty package stands in for the terminal emulator it depends on, so the image has none) with a 32-pixel taskbar along the bottom, which reserves its strip of the screen through `_NET_WORKAREA` as a person's panel does. Nothing is spoofed: Chromium reads the work area from the window manager. The browser window sits at 0,0 with no frame and fills the screen above the taskbar, so Visual-mode coordinates are still screen coordinates, and the taskbar shows in screenshots and the live view. The taskbar shows only a clock, in the exit's time zone; there are no menus, key bindings or window buttons, so nothing on the desktop starts a program or kills Chromium, and the window can't be moved or resized from the desktop. Fullscreen covers the taskbar, as elsewhere.

Measured on Chromium 156 with `TestMeasureWindowAtLoad` (`go test -tags 'docker measure' -run TestMeasureWindowAtLoad -v ./images/chromium/`, a 1280×800 screen), 110 loads of the detection page each, before and after:

| | Before (no window manager) | After (JWM and taskbar) |
|---|---|---|
| `screen.availHeight` | 800 in 110 of 110 loads | **768** in 110 of 110 |
| Window once loaded | 1279×799 (a pixel short of the screen) | 1280×768, the work area exactly |
| 0×0 at the first script: Visual, fresh containers | 14 of 20 | 14 of 20 |
| … Visual, new tabs | 20 of 30 | 20 of 30 |
| … Visual, the same tab again | 24 of 30 | 28 of 30 |
| … API mode, `Page.navigate` | 16 of 30 | 18 of 30 |

So the taskbar hint is gone, and the 0×0 at load is unchanged (see Signals that remain). On CreepJS, in Visual mode on this machine, "like headless" went from 50% to 44%: `noTaskbar` is now false. Of what it still counts, SwiftShader was the container's (gone since PROJ-148, below); the rest (`noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax`: APIs desktop Linux Chromium doesn't have, and the default light theme) aren't about the container. The rate depends on timing: these runs, on 2026-11-12, shared 6 CPUs with other work, and PROJ-15 saw about a third.

## WebGL on llvmpipe (PROJ-148)

WebGL used to run on SwiftShader (`--use-angle=swiftshader --enable-unsafe-swiftshader`), whose renderer string pages read as a headless or server sign. It now runs on **Mesa's llvmpipe**, the renderer of a Linux desktop or VM without a GPU driver. It is still software rendering, and the string says so; it's not spoofed, and there's no fallback.

Measured on 2026-11-17 with Chromium 156.0.8078.79, Debian trixie's Mesa 25.0.7-2+deb13u1 and LLVM 19.1.7, on amd64 only: a 6-vCPU VM on an AMD Ryzen 9 6900HX, under rootless Podman. Both renderers ran on the same host, the same day, through the same **residential** connection (not PROJ-15's OVH datacenter address), with exit-location matching on. The SwiftShader runs used the image before the change, the same Chromium; flags were swapped with a throwaway wrapper during the spike.

**The switches.** `--ignore-gpu-blocklist` alone, with no `--use-angle`, puts WebGL on llvmpipe: ANGLE's default backend on Linux is OpenGL, through Xvfb's GLX (`chrome://gpu`: `GL implementation parts (gl=egl-angle,angle=opengl)`, display type `ANGLE_OPENGL`). No EGL package is needed. Without that switch Chromium blocks llvmpipe: `chrome://gpu` says `WebGL: Disabled`, `getContext('webgl')` returns null, and compositing falls back to software; so that switch is required, and the tests fail with "no WebGL" without it. `--disable-gpu-compositing` keeps page compositing in software (see the cost below); WebGL then reads `Hardware accelerated but at reduced performance` in `chrome://gpu`, and renders as before.

**Packages: none new, and the image is the same size.** `xvfb` already pulls in Mesa's GLX and llvmpipe (`xvfb` → `libgl1` → `libglx0` → `libglx-mesa0` → `mesa-libgallium`, 41 MB, which links `libllvm19`, 124 MB). The Dockerfile now names `libgl1` and `libglx-mesa0`, which Chromium needs, and the build fails if libGL, the Mesa GLX library or the gallium driver is missing, lacks llvmpipe or can't resolve its libraries. That checks the files and their linkage, not an actual load: the image has no GL client to load the driver with at build time (Xvfb reports nothing about GLX as it starts), so the load is checked by the Docker tests' `webgl` subtest. amd64 image: 1,100,647,500 bytes before, 1,100,647,108 after. arm64: not measured here; the dependency chain is the same in Debian.

**The renderer string**, exactly, from `UNMASKED_VENDOR_WEBGL` / `UNMASKED_RENDERER_WEBGL`:

| | Vendor | Renderer |
|---|---|---|
| Before (SwiftShader) | `Google Inc. (Google)` | `ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero) (0x0000C0DE)), SwiftShader driver)` |
| After, amd64 (AVX2) | `Google Inc. (Mesa)` | `ANGLE (Mesa, llvmpipe (LLVM 19.1.7 256 bits), OpenGL 4.5)` |
| After, arm64 | not run here | expected `… (LLVM 19.1.7 128 bits) …`, unchecked |

What varies: the LLVM version (and Mesa's, in `chrome://gpu`) changes with the image's Debian packages, and the vector width comes from the host CPU (256 bits with AVX2, 128 without it, as with SSE4 or NEON). So the string is the same for every profile on a host and image (`TestWebGLRendererIsStable`), changes once when an image update moves LLVM, and can change when a profile moves to a host with a different CPU. WebGL parameters change with it (`MAX_TEXTURE_SIZE` 16384 instead of 8192, line and point ranges, extension counts), and so do WebGL and canvas hashes, once, at the update.

**WebGL1 and WebGL2** both give a context and draw a triangle that reads back exactly (`51,204,102,255` with `readPixels`), and a WebGL canvas shows on the X display, which screenshots and the live stream read, with or without `--disable-gpu-compositing` (`TestBrowserEnvironment/webgl` and `webgl on screen`).

**Sandbox.** `chrome://sandbox` still says "You are adequately sandboxed" (namespaces and seccomp-BPF). `seccomp.json` and the AppArmor profile are unchanged, and llvmpipe's JIT works under BrowserPier's seccomp profile (the AppArmor profiles weren't checked, see below). The GPU process is in a process of its own (`In-process GPU: false`) and has never crashed in these runs. `chrome://gpu` said `Sandboxed: false` for it, **with SwiftShader too**: its `/proc` status showed only the container's seccomp filter (`Seccomp_filters: 1`), where renderers have Chromium's as well (2). Nothing in PROJ-148 changed that; PROJ-165 did (below). AppArmor: this rootless Podman has none (it refuses `apparmor=browserpier`; `unconfined` is the same as its default), so only "no AppArmor" ran here; the `browserpier` and Docker-default choices weren't.

**Cost**, `TestMeasureWebGLCost` (`go test -tags 'docker measure' -run TestMeasureWebGLCost -v ./images/chromium/`), a 1280×800 screen, 20-second samples of the container's cgroup (memory as `docker stats` counts it, which the RAM limit uses; CPU in percent of one core). The WebGL page redraws 40 large, blended, full-window triangles a frame; the heavy site is a long Wikipedia article scrolled through Visual mode every half second; "watched" reads the live stream at the same time.

| | SwiftShader | llvmpipe, GPU compositing | llvmpipe, `--disable-gpu-compositing` (chosen) |
|---|---|---|---|
| First page loaded, from `docker run` | 1.5–2.0 s | 1.2–1.6 s | 1.6–1.8 s |
| Idle page: CPU, memory | 7–8%, 238–240 MB | 7–8%, 242–246 MB | 7%, 220–222 MB |
| WebGL page | 48 fps, 376% CPU, 314–321 MB | 62 fps, 274% CPU, 300 MB | 63 fps, 240–281% CPU, 294–299 MB |
| WebGL page, watched | 386% CPU, 397–401 MB | 297% CPU, 385 MB | 265–300% CPU, 385–387 MB |
| Heavy site, scrolled | 83–84% CPU, 390–401 MB | 34–35% CPU, 379–381 MB | 20% CPU, 342–381 MB |
| Heavy site, watched | 83–85% CPU, 484–487 MB | 47–48% CPU, 472–503 MB | 34–37% CPU, 425–476 MB |
| GPU process | 120–185 MB, 20–22 threads | 197–252 MB, 25–26 threads | 159–239 MB, 24 threads |

The frame rates here were counted over the 20-second window plus the readings that followed it (a second or more), so they read high: the 63 fps is the 60 fps frame cap. Every column was read the same way, so the comparison holds. The measure now times the window alone (after the PR #79 review).

With 3,000 triangles every core is busy either way: SwiftShader drew 2 frames a second, llvmpipe 4. So llvmpipe draws WebGL faster for less CPU, and the container's memory stays where it was, well inside SPEC §3's 1–2 GB a browser. Compositing pages in software as well cut the CPU of scrolling an ordinary page by about 40% against compositing them through llvmpipe (and by three quarters against SwiftShader, which composited every page), so the image passes `--disable-gpu-compositing` too.

**Threads.** llvmpipe starts one rasteriser thread per CPU the process may run on (6 here; 2 with Chromium pinned to two CPUs), where SwiftShader started 6 whatever the pinning. Pinned to two CPUs, llvmpipe drew the WebGL page at 60 fps for 183% CPU, SwiftShader at 24 fps for 187%; `LP_NUM_THREADS=1` cut llvmpipe to 36 fps and saved 7 MB. The threads sleep unless a page draws WebGL, and a container's CPU limit caps what they can take in total, so PROJ-148 left `LP_NUM_THREADS` unset; PROJ-165 sets it to 0 for the GPU sandbox (below). It doesn't show in the renderer string; a page sees only `navigator.hardwareConcurrency`, which follows the CPUs and not llvmpipe, and how fast it draws.

**Detection pages**, Visual mode (the address bar through xdotool, screenshots; each page's text read over CDP only after it had shown its results), same connection, before and after:

| Page | SwiftShader | llvmpipe |
|---|---|---|
| bot.sannysoft.com | All pass except **WebGL renderer** (it fails anything containing "Swift") | **All pass**, the 19 fingerprint-scanner checks too |
| CreepJS | 0% headless, 0% stealth, **44% like headless** (`hasSwiftShader: true`); GPU confidence "moderate" | 0% headless, 0% stealth, **38% like headless** (`hasSwiftShader: false`); GPU confidence "high" |
| BrowserScan bot detection | Normal | Normal |
| BrowserScan fingerprint | Authenticity 100%, bot detection No | Authenticity 100%, bot detection No |
| deviceandbrowserinfo: Are you a bot? | "You are human!", every signal false | "You are human!", every signal false, `isWebGLInconsistent` and `hasInconsistentGPUFeatures` included |

What CreepJS still counts in "like headless" is `noWebShare`, `noContentIndex`, `noContactsManager`, `noDownlinkMax` (APIs desktop Linux Chromium doesn't have) and the default light theme (`hasKnownBgColor`, `prefersLightColor`); none comes from the container now. **Decision: go**, on amd64: sannysoft's WebGL line and CreepJS improved, nothing got worse, and it costs less.

**Existing profiles.** `TestLoginSurvivesImageUpgrade` signed in with the 0.2.1 image (SwiftShader), then started that profile, its `GPUCache` folders included, on the new image: it kept the login, WebGL worked on llvmpipe, and the GPU process didn't fail. The renderer, and with it WebGL and canvas hashes, changes once for such a profile.

**Not run here, so unverified:** arm64 (CI builds the arm64 image under emulation, and the build-time library check runs there; no Docker test has run on arm64, so WebGL on llvmpipe there is unchecked, and since there's no fallback, an arm64 failure would mean no WebGL at all: run `TestBrowserEnvironment` and `TestWebGLRendererIsStable` on an arm64 host or the Umbrel before a release ships this), the Umbrel, llvmpipe's JIT under the `browserpier` and Docker-default AppArmor profiles (CI's Docker job, on Docker with AppArmor, covers one of them once the branch runs there), and PROJ-15's OVH datacenter address. The **go** above is for amd64 without AppArmor.

`TestVisualModeLooksLikeAPerson`, `TestPageToolsStayQuiet`, `TestContentStaysQuiet`, the login tests and `TestMeasureWindowAtLoad` (window 1280×768 and `availHeight` 768 in all 70 loads, as in PROJ-29's table) pass unchanged.

## The GPU process's own sandbox (PROJ-165)

Until PROJ-165 the GPU process, which handles pages' WebGL calls, shaders and compositing, ran under the container's seccomp filter, with no capabilities and no new privileges, but without Chromium's own GPU seccomp policy: `chrome://gpu` said `Sandboxed: false`, with SwiftShader as with llvmpipe. Measured on 2026-11-18, same host and Chromium 156.0.8078.79 as above.

**Why.** On Linux Chromium starts the GPU process's sandbox after GL has started (the late start is its default outside ChromeOS), and `SandboxLinux::InitializeSandbox` can only start it in a single-threaded process. In a GPU process with other threads it logs `WARNING:sandbox/policy/linux/sandbox_linux.cc:405] InitializeSandbox() called with multiple threads in process gpu-process.` and goes on without it; for every other process type that's fatal. Here GL starting meant Mesa starting threads: llvmpipe's rasterisers (`llvmpipe-0` to `llvmpipe-5`, one per CPU) and the shader disk cache's writer (`chromiu:disk$0`). SwiftShader, before PROJ-148, started its own worker threads the same way. So it isn't Debian's build, the zygote, the container's `seccomp.json`, `/dev/shm` or the missing `/dev/dri`: a Linux desktop whose Mesa driver starts threads as GL starts would be the same (not checked on a desktop VM here; the code path is Chromium's own).

**What was tried**, each with a second Chromium in a running container, its GPU process's threads and `Seccomp_filters` read from `/proc`:

| Chromium's environment and switches | GPU process | WebGL |
|---|---|---|
| as before | 24 threads, `Seccomp_filters: 1`, the warning above | llvmpipe |
| `LP_NUM_THREADS=0` | 12 threads (the disk-cache one still there), 1 filter, the warning | llvmpipe |
| `LP_NUM_THREADS=0 MESA_SHADER_CACHE_DISABLE=true` (chosen) | 11 threads, **2 filters**, `Activated seccomp-bpf sandbox for process type: gpu-process` | llvmpipe, draws and reads back |
| `--gpu-sandbox-start-early` | sandboxed before GL, then `ANGLE Display::initialize error 12289: Could not open the default X display`, GL fails | none |
| `--gpu-sandbox-failures-fatal=yes`, no settings | `FATAL: Current process is not mono-threaded!` after 30 tries, three crashes, then a GPU process without GL, sandboxed | none |

Starting the sandbox early can't work without widening Chromium's GPU policy, which is Chromium's code: ANGLE opens its own connection to the X display as GL starts, and the sandbox no longer lets it.

**What the image does.** The entrypoint gives Chromium `LP_NUM_THREADS=0` (llvmpipe rasterises on the GL thread itself) and `MESA_SHADER_CACHE_DISABLE=true` (Mesa's on-disk shader cache sat under the container's `HOME`, lost with the container, and the sandbox would keep it from opening its files; Chromium's own `GPUCache` in the profile is unchanged), and passes `--gpu-sandbox-failures-fatal=yes`, so a GPU process that can't sandbox itself exits instead of running on: after its restarts Chromium falls back to a GPU process without GL, sandboxed, and pages get no WebGL. The image build fails if Mesa's `libgallium` doesn't know either setting (their names aren't in it); whether they work, the Docker test shows. If the sandbox fails at run time anyway, the entrypoint logs one warning, `a GPU process exited because it couldn't start its sandbox; Chromium restarts it, and if that keeps failing, pages get no WebGL`. It waits for the FATAL `Current process is not mono-threaded!` that follows Chromium's `InitializeSandbox() called with multiple threads in process gpu-process` line, since Chromium first waits for the threads to end and the sandbox may then start after all, so the admin reads it in the browser's log; nothing attaches to the page for it. `seccomp.json` and the AppArmor profile are unchanged, and llvmpipe's JIT works inside Chromium's GPU policy. `chrome://gpu` now says `Sandboxed: true`, the GPU process's `/proc` status `Seccomp_filters: 2`, and `TestBrowserEnvironment/gpu` requires both; the Docker tests fail on the warning above too.

**Fingerprint.** Unchanged: the renderer string, WebGL parameters and extensions don't depend on llvmpipe's thread count, and `navigator.hardwareConcurrency` follows the CPUs. A page could only see that WebGL draws more slowly.

**Cost**, `TestMeasureWebGLCost` with the same 40-triangle WebGL page (light: the old setup had cores to spare, so its 63 fps is the frame cap) and with 3,000 triangles (`MEASURE_TRIANGLES=3000`, which kept every core busy before), before and after:

| | Before (llvmpipe, threads) | After (one thread, sandboxed) |
|---|---|---|
| First page loaded, from `docker run` | 1.9 s | 1.2–1.8 s |
| Idle page: CPU, memory | 7%, 220 MB | 7%, 222 MB |
| WebGL page | 63 fps, 244% CPU, 297 MB | 29–34 fps, 117–119% CPU, 289–290 MB |
| WebGL page, watched | 268% CPU | 139–141% CPU |
| WebGL page, 3,000 triangles | 4.2 fps, 568% CPU | 0.8 fps, 107% CPU |
| … watched | 560% CPU | 125% CPU |
| Heavy site, scrolled | 19% CPU | 19–20% CPU |
| Heavy site, watched | 35% CPU | 36% CPU |
| GPU process | 24 threads | 11 threads |

WebGL gets one core instead of every core. On the light page that halves the frame rate and the CPU; on a scene that used every core it is about as many times slower as the host has cores (5 times here, on 6; more on a bigger host), for a fifth of the CPU. That also leaves the other cores to the host's other browsers. Ordinary pages, which are composited in software and don't touch llvmpipe, cost the same. **Decision: go**: the GPU process parses what pages send it, and AGENTS.md's rule is that Chromium keeps its sandbox.

**Not run here:** **arm64** can't run on this amd64 VM, so whether Mesa on arm64 starts no thread before the sandbox is unchecked. If it does start one, `--gpu-sandbox-failures-fatal` means pages there get no WebGL at all rather than an unsandboxed GPU process; CI builds the arm64 image (and its library check) under emulation but runs no Docker test on it. Run `TestBrowserEnvironment` and `TestWebGLRendererIsStable` on an arm64 host (the Umbrel, if it's arm64) before a release ships this: PROJ-187, with PROJ-148's arm64 check. Also not run: the Umbrel, the `browserpier` and Docker-default AppArmor profiles (this rootless Podman has no AppArmor), as for PROJ-148; and a desktop VM with Debian's Chromium on llvmpipe, to compare. `TestBrowserEnvironment`, `TestWebGLRendererIsStable`, the login tests, `TestLoginSurvivesImageUpgrade` from the 0.3.0 image (SwiftShader), `TestSeccompAllowsOnlySandboxNamespaces` and `TestAppArmorProfile` pass.

## Continue where you left off (PROJ-112)

Chromium runs with `RestoreOnStartup: 1`, so it keeps session-only cookies across a stop and reopens its tabs as it starts. That's a common setting in people's browsers. A page sees nothing new: a restored tab loads as it would after a person reopens their browser, and a tab left on a form's answer isn't posted again (`TestRestoredPostPageIsNotResubmitted`). `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged with it.

## Ad and tracker blocking (PROJ-119)

Blocking happens in the container's SOCKS forwarder, by host name, before anything leaves the container: Chromium gets SOCKS code 2 for a listed host, and the request fails as any unreachable host's does (`net::ERR_SOCKS_CONNECTION_FAILED` in the network log). Nothing is injected into pages, no extension is installed, and no CDP is attached, so Visual mode stays clean and the fingerprint is unchanged. What a page can see is what it sees with any DNS-level blocker (Pi-hole, NextDNS, a hosts file), which many people run: its ad and analytics scripts don't load. Sites that check for that ("please disable your ad blocker") may say so; challenge and CAPTCHA providers are on a built-in allowlist, so blocking never removes a challenge's own scripts, and a site that insists can be opened with `block_ads: false` for the session.

`TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass with blocking on (2026-11-15, `localhost/bp-agweb-chromium:proj119`): every real-API Docker test's browser now blocks, with a small list of real ad domains, and both tests check their session does. The detector reports the same as before. **Decision: nothing to accept.**

## Input that moves like a hand (PROJ-113)

Before PROJ-113, Visual mode's pointer jumped (`mousemove X Y click`), pressed and released at once, and typed every key 12 ms apart (about 83 characters a second); act moved once to the element's exact centre and pressed with no hold. Pages that score behaviour read exactly that. Now, at human pace (the default; `pace: "instant"` keeps the old input), both modes follow `internal/motion`, a Go port of agweb's Motion model (MIT, see NOTICE):

- **The pointer** glides from where it is (read from X in Visual mode; where act last left it in API mode, or in from an edge of the window at first) along a bowed cubic Bézier with a wobble of about a pixel, at 60 Hz, on minimum-jerk timing whose speed peaks early, in a Fitts's-law time (0.2–1.15 s); hard throws fall short (or now and then overshoot) and correct.
- **The aim** is scattered around the target's middle (SD a sixth of its size, never past 40% of it): in Visual mode, which sees only a point, 8 px (within about 3 px, so a small checkbox or a dense row isn't missed), while the glide's timing assumes 44 px; the element's own box for act, whose chosen point goes through the same hit test as the centre (and must not be on a control inside the element, such as a row's Delete button), and again just before the press.
- **Buttons and keys** go down after a pause (median 0.16 s) and are held about 116 ms. **Typing** has log-normal gaps (median 0.2 s), longer after spaces and punctuation, shorter when the hands alternate, with a hesitation now and then. **Scrolling** turns the wheel in notches that come faster mid-turn.

What a page's own script records (`browsertest.Detector`'s input recorder, `InputURL`), in `TestVisualInputLooksHuman` and `TestActInputLooksHuman`, 2026-11-15, Chromium 156:

| | Visual mode | act (API mode) |
|---|---|---|
| A 400 px click: moves before the press | 45, up to 11 px off the straight line | 43, up to 18 px off it |
| Button held | 60–200 ms | 60–200 ms |
| Ten clicks at one point | scattered, within 3 px | scattered, all on the button |
| Key-down gaps, 30 characters | mean 0.28 s, CV 0.41 | mean 0.20 s, CV 0.44 |
| `isTrusted` | true for every event | (not checked; CDP's are trusted too) |

None of this is fingerprint spoofing: the browser reports nothing different about itself. `TestVisualModeLooksLikeAPerson` and `TestPageToolsStayQuiet` pass unchanged with it. Not done: typing mistakes and corrections, and a check against a public behaviour-scoring page (to be done by hand).

## Fixed along the way

- **`Intl` contradicted the browser's languages.** The image had only Chromium's `en-US` interface, so whatever `LANGUAGE` said, `Intl` reported `en-US` while `navigator.languages` and Accept-Language followed the exit location (`fr-FR`). The image now installs `chromium-l10n` (about 120 MB), and its build fails if the locales are missing. `Intl` now follows the interface language: `de` for a German exit, as Chromium on a German Linux desktop reports.

## API-mode page tools (PROJ-21)

BrowserPier's own page tools (`snapshot`, `find`, `act`, `wait_for`, `navigate`, `tabs`) drive API-mode browsers over CDP from the orchestrator. They are built to stay quieter than Playwright: one short-lived browser-level connection per call through the control socket, **no domain is ever enabled**, scripts run only in an isolated world, and nothing is written into the page. The allow-list is in `internal/pagetools`; the CDP helper that enforces it is `internal/cdp`.

### Phase 0: what works without any `*.enable` (2026-11-12, Chromium 156.0.8078.79)

Measured by `TestSpike` (`internal/cdp/spike_docker_test.go`, run with `BROWSERPIER_SPIKE=1`) through the control socket, sending only the tools' allow-list.

| Question | Result |
|---|---|
| `Accessibility.getFullAXTree`, `DOMSnapshot.captureSnapshot`, `DOM.describeNode`, `DOM.resolveNode` (into an isolated world), `DOM.getContentQuads`, `DOM.scrollIntoViewIfNeeded`, `DOM.focus`, `Page.createIsolatedWorld`, `Runtime.callFunctionOn` / `releaseObject` on isolated objects | All work with nothing enabled, in a few milliseconds on a small page. `Accessibility.getPartialAXTree` (one node, no relatives) works too, and the tools use it to echo what a step hit. So does `DOM.setFileInputFiles` by backend node (PROJ-117, `TestPageToolsUpload`), act's upload step. |
| `Page.handleJavaScriptDialog` | **Doesn't work** without `Page.enable` ("No dialog is showing", even with an `alert()` open). So the tools have **no `dialog` step**. |
| A source for an open dialog's type and message, without `*.enable` | **None.** `Target.getTargets` and `/json/list` don't mention it. While an `alert()` is open, every renderer-side method (`Page.getFrameTree`, `Runtime.evaluate`, `Page.createIsolatedWorld`, the AX tree, the DOM snapshot) gets no answer; browser-side ones (`Page.getNavigationHistory`, `Target.getTargets`) still answer. The tools use that difference to say "the page is busy or has a dialog open; if it stays that way, request a handoff." |
| Keys after `Page.navigate` (focus in the address bar) | `DOM.focus` moves the page's focus and `document.hasFocus()` turns true. Right after a navigation, Chromium drops events until the first raw key-down, so a CDP `keyDown` with text can lose its first character ("abc" arrives as "bc"). Sent as a keyboard sends them, `rawKeyDown`, `char`, `keyUp`, the text always arrives in full; the page can still miss the very first `keydown` event. The tools type that way. |
| `Input.insertText` right after `Page.navigate` | Reaches the focused field in full (7/7 characters into a password field). `fill`'s replace, the `selectAll` editing command then `insertText`, works too. |
| The foreground tab, 20 tabs | `document.visibilityState` (read in an isolated world) is `visible` in exactly one tab: the one created last, or the one `Target.activateTarget` named. Reading all 20 takes 30–80 ms on one connection. A popup window (`window.open` with features) is `visible` and its opener turns `hidden`; a `target=_blank` link opened from a CDP click left its opener in front. Measured without a window manager, and the same again under PROJ-29's JWM (2026-11-14, `BROWSERPIER_SPIKE_ONLY=tabs`; 60–120 ms to read all 20 on a busier machine). |
| Large pages (AX tree / DOM snapshot) | 4,000 elements: 4.0 MB in 0.3 s / 0.9 MB in 0.1 s. 20,000: 20 MB in 2.0 s / 4.9 MB in 0.4 s. 80,000: 81 MB in 28 s / 20 MB in 1.7 s. 200,000: neither answered within 60 s. A real long page (Wikipedia's population list, 10,000 elements): 8.0 MB in 0.5 s / 3.4 MB in 0.3 s. The HTML spec (340,000 elements): no AX tree in 60 s; DOM snapshot 99 MB in 9 s. **The cap:** the tools take the DOM snapshot first, and a page with more than 60,000 DOM nodes (about 30,000 elements) is too large for the AX tree; `snapshot` says so and `find` reads the DOM snapshot's text alone. Messages are capped at 32 MiB, and at most two whole-page reads run at once. |
| Isolated-world build-up | 1,000 calls on one document (each: dial, attach, `getFrameTree`, `createIsolatedWorld`, `evaluate`, close) got the **same** execution context every time (Chromium keeps one world per name and document), with no measurable growth in renderer memory (1,876 → 1,901 MB on an already-large renderer, within noise). About 6 ms of overhead per call. |
| Anything left attached | After the connection closes, `Target.getTargets` shows the page `attached: false`. |
| The front tab, without attaching (PROJ-118, 2026-11-15) | `Target.getTargets` lists tabs in an order of its own, the same whichever tab `Target.activateTarget` brought to front; `/json/list` lists them most recently activated first. A handoff's `until_url` watch, which may not attach while a person has the browser, sends only `Target.getTargets` every 500 ms and so doesn't know the front tab: it counts any tab that has navigated since the handoff opened (SPEC §7). |

### The continuous detector

`browsertest.Watch` (`internal/browsertest/watch.go`) is a page that keeps checking, every 200 ms on its main thread and in a worker, for what pages use to spot CDP automation: the `Error.stack` console leak of an enabled `Runtime`, a logged `Proxy` being inspected, console calls getting slow, `navigator.webdriver` and automation globals; hooks on the DOM methods automation calls in the page's own world (`querySelector`, `getBoundingClientRect`, `focus`, `elementFromPoint`…); and input tells (screen position equal to client position, a click with no mouse movement, a key event without a code, text with no key pressed); and, since PROJ-117, file inputs (an untrusted `input` or `change` event, and files set with no click on the input or a label in the minute before, `file-without-click`). It is shared with PROJ-57.

Console calls getting slow (`console-slow`: 50 `console.debug` calls costing five times what they did at load, and over 50 ms) is timing, so a starved machine trips it as well: on CI runners and on a workstation with its CPUs busy, the page's checks took 5–30 ms now and then, with or without calls. The tests run each batch through `Watch.Quiet`, which, when a batch's only hits are `console-slow`, runs a control batch of as many sleeps with no calls on the same page, and runs the batch again if the control is quiet; slow in the control too, with nothing attached, is the machine's. Measured on Chromium 156, `Runtime.enable` held through 20 calls doesn't make the checks slow at all, so the 50 ms floor (5 ms before 2026-11-14) costs no detection that the positive control shows.

**Positive control** (`browsertest.WatchPositiveControl`: what Playwright does when it connects and acts): detected, through main-world calls (`querySelector`, `getBoundingClientRect`, `focus`), its binding (`__playwright__binding__`), a click with no movement and text with no key pressed. `Runtime.enable` **alone** is no longer detected by the console checks on Chromium 156 (the `Error.stack` leak is closed; getters on an Error's `message` or `name` can't be used, as Chromium's console reads them with nothing attached).

**The tools' CDP calls, 20 runs each, nothing else attached:** nothing recorded, for reading (frame tree, AX tree, partial AX tree, DOM snapshot, isolated world), clicking (scroll into view, quads, hit-test in an isolated world, mouse move, press and release), `fill` (focus, `selectAll`, `insertText`), `type` (focus, key events) and tabs (targets, visibility of each).

**Accepted residuals** (each is what Playwright does as well, and API mode is the mode that isn't for protected sites):
- `fill` puts its text in with one `insertText` after a select-all key press: a detector that checks that typed text matches the keys pressed can tell. Typing with `type` sends one key per character.
- Key events come as fast as CDP sends them, with no typing rhythm.

**The tools themselves, 20 runs each** (`TestPageToolsStayQuiet`, `internal/api/page_tabs_docker_test.go`, in `make test-docker`), nothing else attached: nothing recorded for `snapshot`, `find`, `act` (click, fill, type, press and hover, each its own run), `wait_for` (text, a ref, and `load`, which reads the page's Resource Timing entries from the isolated world, PROJ-122), act and `wait_for` with a role target (PROJ-123: hover, click and wait, each its own run; resolving one is the snapshot's read of the AX tree and DOM snapshot, nothing in the page), `tabs` and `navigate` (reload), while the positive control in the same test was caught (main-world calls and its binding). So there is no hit to decide on; the residuals above still stand.

**act's upload step** (PROJ-117, 2026-11-15, Chromium 156): `DOM.setFileInputFiles` on the input's backend node, with `/files/uploads/<name>` paths. Chromium sets the files and fires `input` then `change` itself, both trusted (`isTrusted`), as when a person picks the files in the chooser; no chooser opens and no script of BrowserPier's touches the page's world. 20 runs on the detector's file input recorded one hit, `file-without-click@main` (the first run's `change`; setting the same file again fires nothing), and no `untrusted-event` or main-world call. **Decision: accepted** and allowed for that batch in `TestPageToolsStayQuiet`. A person opens the chooser with a click on the input or its label, so a page can see files arrive with no click before them; but a person who drops files on the input sends none either, Playwright's `setInputFiles` does the same, and API mode isn't the mode for protected sites. Clicking first to make it look right isn't possible: the click would open the chooser, a window of the browser's own that nothing over CDP can close.

**act's re-centre on a covered element** (PROJ-115): only when a pointer step's hit test finds the element covered (or off screen), act calls the element's `scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'})` once, as a function on its isolated-world object, then keeps hit-testing in the isolated world for up to `cover_wait_ms`. Nothing runs in the page's world; what the page can see is a scroll with no wheel or key event before it, as with `DOM.scrollIntoViewIfNeeded` which every pointer step already sends. The waiting adds no input and nothing the page can see. `TestPageToolsStayQuiet`'s clicks aren't covered, so it never takes this path. **Decision: accepted**, like the scroll into view.

**The Cloudflare challenge look** (PROJ-120, for PROJ-25): the page-info read that `snapshot`, `find`, `navigate` and `page_content` already make in the isolated world also runs a `querySelector` for a mark of Cloudflare's challenge page (`#challenge-form`, `#challenge-stage`, a challenge-platform script or iframe, each one's `src` parsed with `URL` to keep only the page's own origin) and reads `document.title`, then, only on a page with one, `querySelectorAll('input[id^="cf-chl-widget-"]')` and each input's `value`, and, only when that finds the challenge widget's input, its parent's `getBoundingClientRect()` and the window's `devicePixelRatio`, `outerWidth`/`outerHeight` and `screen` size. All of it is in the isolated world, on the browser's own functions: the page's hooks on `querySelector` or `getBoundingClientRect` are in its own world and never run; a forced layout is all a page could notice, as for any read of a box. `TestPageToolsStayQuiet` (2026-11-15) still records nothing for these tools; its detector page has no widget, so the box read isn't in it. The checkbox itself is clicked, if at all, after the agent has checked a screenshot, through xdotool by the agent's `/computer` call or by a person, never over CDP. **Decision: nothing to accept.**

**act's changes** (PROJ-114): after a snapshot, act reads the page again after its steps, to answer with what changed. The read is `snapshot`'s (frame tree, isolated world, DOM snapshot, AX tree) plus one more `Page.getFrameTree` to check the document didn't change during it; nothing new reaches the page, and the diff itself is worked out in the orchestrator. `TestPageToolsStayQuiet` takes a snapshot before its act runs, so every act batch there includes the read, and has a batch with `changes: true` and one of `snapshot` with `diff: true`. **Decision: nothing to accept**, the reads were already measured quiet.

## Page content (PROJ-20)

`POST /api/sessions/{id}/content` (MCP `page_content`) reads the active tab as Markdown or its main text, takes a screenshot or prints a PDF. Its CDP calls go through the same per-call connection as the page tools, with their own allow-list (`contentAllowList` in `internal/pagetools`), and nothing is enabled.

**Measured** on Chromium 156.0.8078.79, 2026-11-13, with a page listening for `beforeprint`, `afterprint`, the print media query, `resize` and `visibilitychange`:

| Call | What the page saw |
|---|---|
| `DOMSnapshot.captureSnapshot` (Markdown and main text) | Nothing. The snapshot walks the flat tree, so shadow roots and slotted content come in place. |
| `Page.printToPDF` | `beforeprint`, the print media query matching, `afterprint`, and the query no longer matching, on every call; no resize. It works in the headful browser (about 25 ms for a small page, under a second for a news front page). |
| `Page.captureScreenshot` | Nothing (viewport only). Not used: screenshots come from the X display, with no CDP at all, in both modes. A full-page capture (`captureBeyondViewport`) resizes the page, which it can see. |

**The content calls, 20 runs each** (`TestContentStaysQuiet`, `internal/api/content_docker_test.go`, in `make test-docker`), nothing else attached, against the continuous detector, which now also reports printing (`print`): nothing recorded for `markdown`, `readability` and `screenshot`; `pdf` recorded `print` and nothing else. The positive control in the same test was caught.

**Decision:** a PDF is a print, and the page sees what it sees when a person presses ctrl+P. Printing is something people do, so it isn't a sign of automation by itself; a page could notice a print with no key pressed before it. Accepted: an agent on a site that watches for it reads the page as Markdown instead, and Visual mode never prints.
