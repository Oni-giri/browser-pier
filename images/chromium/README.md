# Browser image

`ghcr.io/oni-giri/browser-pier-chromium`: headful Chromium on Xvfb with a minimal desktop (JWM and a taskbar, `jwmrc`), xdotool for OS-level input, GStreamer sidecars that stream the display as H.264, live and, for session recordings, as fragmented MP4, and in Visual mode a private accessibility bus that the entrypoint reads the page's element tree from. See SPEC.md §3 for why it's built this way.

```sh
make image-chromium   # build from the repo root
make test-docker      # build, then run the tests that need Docker against it
```

## Running it

The orchestrator starts these containers; nothing else should. Each one needs:

| What | How | Why |
|---|---|---|
| Seccomp profile | `--security-opt seccomp=seccomp.json` (embedded as `chromium.SeccompProfile`) | Chromium's sandbox needs user namespaces, which Docker's default profile blocks. Without it Chromium won't start. |
| AppArmor profile | Docker's, or `--security-opt apparmor=browserpier` where Docker's refuses user namespaces (AppArmor 4, Debian 13, with Docker 29.1 or earlier) | `apparmor/browserpier` is Docker's profile plus `userns`. The image carries it, and `apparmor/load`, at `/usr/share/browserpier/apparmor/`, for the compose file's loader service; the Umbrel app's `hooks/pre-start` carries copies, written by `go generate -run gen_umbrel_hook ./images/chromium`. The orchestrator picks the profile by running the image with the argument `sandbox-check`, which tests the namespaces and the chroot Chromium's sandbox needs and exits 0 if they work; the image label `io.browserpier.sandbox-check=1` says the image has it. See "AppArmor" in `deploy/README.md`. |
| Capabilities | `--cap-drop ALL --cap-add SYS_CHROOT`, `--security-opt no-new-privileges` | The browser runs as a non-root user with no effective capabilities. The sandbox chroots inside its own user namespace, which needs `SYS_CHROOT` in the bounding set; without it the zygote dies at startup. |
| User | `--user UID:GID` (any non-root uid) | The orchestrator runs it as its own uid so it owns the profile files. `HOME` is `/tmp/home`, created at startup. |
| Profile folder | mounted at `/profile` | Chromium's user data dir, writable by that user. |
| Control socket dir | mounted at `/run/browserpier`, shared only with the orchestrator | The entrypoint creates `control.sock` there. |
| Files folder | mounted at `/files`, with `downloads/` and `uploads/` in it | A managed policy sends every download to `/files/downloads` without asking; the orchestrator puts uploads in `/files/uploads`. |
| Stop timeout | `docker stop -t 30` (or `StopTimeout` in the API) | The entrypoint closes Chromium on SIGTERM. Docker's default of 10 s is too short to rely on. |
| Shared memory | `--shm-size=1g` | Under 512 MiB the entrypoint falls back to `--disable-dev-shm-usage`, which is slower. |
| No published ports | — | Everything goes through the control socket. |

Settings, all optional:

| Variable | Default | |
|---|---|---|
| `BROWSERPIER_SCREEN` | `1920x1080` | Display size. The window fills it but for the 32-pixel taskbar along the bottom. |
| `BROWSERPIER_STOP_TIMEOUT` | `20s` | How long Chromium gets to close before it's killed. Keep it under the container's stop timeout. |
| `BROWSERPIER_PROFILE_DIR` | `/profile` | |
| `BROWSERPIER_CONTROL_SOCKET` | `/run/browserpier/control.sock` | |
| `BROWSERPIER_PROXY_FILE` | `/run/browserpier/proxy.json` | `{"url": "socks5://user:pass@host:port"}` (or `http`, `https`): the profile's proxy. Read and deleted before Chromium starts. |
| `BROWSERPIER_GEOIP_URL` | none | Endpoint to look up the exit location (`country_iso`, `time_zone`); empty for no lookup. |
| `BROWSERPIER_GEO_COUNTRY`, `BROWSERPIER_GEO_TZ` | none | The location the profile ran with last time; the zone is kept while the country is the same. |
| `BROWSERPIER_TZ_OVERRIDE`, `BROWSERPIER_LOCALE_OVERRIDE` | none | The profile's manual time zone and language; they win. |
| `BROWSERPIER_MODE` | `api` | `visual` runs Visual mode's accessibility bus (a private `dbus-daemon` on a path socket in `/tmp/browserpier-a11y-<uid>/`, started before Chromium and stopped after it), adds `--force-renderer-accessibility=basic`, points Chromium at the bus (`AT_SPI_BUS_ADDRESS`, `ACCESSIBILITY_ENABLED=1`) and serves `/tree` (PROJ-150). `api` changes nothing. |
| `BROWSERPIER_MOUNT_CHECK` | none | When set, both `/profile` and the control socket's folder must hold a `.browserpier-mount` file with this value, or the entrypoint exits 1 before touching the profile. The orchestrator sets it so a wrong host path can't open an empty profile. |

The container exits 0 only after a graceful stop: Chromium closed through CDP's `Browser.close` in time, so the profile is safely on disk. It exits 1 if Chromium crashed, failed to start, had to be killed, or had to be stopped with SIGTERM because CDP didn't answer (which can lose the last few seconds of cookie writes); the last log line says which.

## Control socket

HTTP over the Unix socket:

| Path | |
|---|---|
| `GET /healthz` | `ok` once Chromium answers on CDP. The socket only appears once it does. |
| `/cdp/...` | CDP over HTTP and WebSocket, relayed to Chromium's loopback port (`/cdp/json/version`, `/cdp/devtools/browser/...`). URLs in CDP's JSON still say `ws://127.0.0.1:9222`; rewrite them. |
| `GET /screenshot` | The display as PNG; `?width=` scales it down. `X-Screen-Width` and `X-Screen-Height` give the screen size. |
| `POST /input/click` | `{"x", "y", "button", "double"}` in screen pixels, through xdotool. |
| `POST /input/type` | `{"text"}`, up to 2000 characters, through xdotool; `\n` presses Enter. |
| `POST /input/events` | The live view's mouse and keyboard: a JSON array of `{"t": "move", "x", "y"}`, `{"t": "down"/"up", "b": 1-3}`, `{"t": "wheel", "dx", "dy"}`, `{"t": "keydown"/"keyup", "k": "<X keysym>"}`, fed to one long-running `xdotool`. |
| `POST /input/release` | Lets go of any key or button the live view still holds. |
| `GET /tree` | Visual mode only: the active tab's page from the accessibility bus, as JSON (document URL, title and box; each node's AT-SPI path, parent, depth, role and state numbers, name, a field's text, boxes in screen pixels, a few attributes; a password field's `filled`, never its text). `?nodes=`, `?depth=`, `?string=` and `?ms=` lower the limits (20,000 nodes, depth 128, 1,000 bytes, 2 s); a read cut short says why in `cut`. 404 when no page is showing, 504 when nothing was read in time, 502 when the bus fails. One read at a time. Read-only: see `readMethods` in `entrypoint/tree.go`. |
| `GET /tree/node` | Visual mode only: `?app=&doc=&id=`, one element read again for a click by ref: its role, states, name and box now, `stale` if it or its document is gone, and what Chromium's hit test finds at its centre when it isn't the element. |
| `GET /geo` | The location the browser runs with: country, time zone, locale, Accept-Language list, and where they came from. |
| `GET /proxy/stats` | Bytes sent and received through the proxy, when there is one. |
| `GET /stream` | The display as an H.264 Annex-B byte stream (constrained baseline, 30 fps, keyframe every 2 s), one reader at a time (409 otherwise). The encoder runs only while it's read. |
| `GET /record` | The display as fragmented MP4 to keep (PROJ-23): at most 5 fps, 3/4 of the screen, H.264 High at constant quality, one keyframe at the start, a fragment about every frame. A pipeline of its own, one reader at a time (409 otherwise), run only while it's read; the live stream is unchanged. |

## Seccomp profile

`seccomp.json` is Docker's default profile with `clone` and `unshare` allowed. Regenerate it with `go generate ./images/chromium` after bumping the pinned commit in `gen_seccomp.go`.
