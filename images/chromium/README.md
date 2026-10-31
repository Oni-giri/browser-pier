# Browser image

`ghcr.io/oni-giri/browser-pier-chromium`: headful Chromium on Xvfb, xdotool for OS-level input and a GStreamer sidecar that streams the display as H.264. See SPEC.md §3 for why it's built this way.

```sh
make image-chromium   # build from the repo root
make test-docker      # build, then run the tests that need Docker against it
```

## Running it

The orchestrator starts these containers; nothing else should. Each one needs:

| What | How | Why |
|---|---|---|
| Seccomp profile | `--security-opt seccomp=seccomp.json` (embedded as `chromium.SeccompProfile`) | Chromium's sandbox needs user namespaces, which Docker's default profile blocks. Without it Chromium won't start. |
| Profile volume | mounted at `/profile` | Chromium's user data dir. A fresh named volume inherits the right owner (uid 1000). |
| Control socket dir | mounted at `/run/browserpier`, shared only with the orchestrator | The entrypoint creates `control.sock` there. |
| Stop timeout | `docker stop -t 30` (or `StopTimeout` in the API) | The entrypoint closes Chromium on SIGTERM. Docker's default of 10 s is too short to rely on. |
| Shared memory | `--shm-size=1g` | Under 512 MiB the entrypoint falls back to `--disable-dev-shm-usage`, which is slower. |
| No published ports | — | Everything goes through the control socket. |

Settings, all optional:

| Variable | Default | |
|---|---|---|
| `BROWSERPIER_SCREEN` | `1920x1080` | Display and window size |
| `BROWSERPIER_STOP_TIMEOUT` | `20s` | How long Chromium gets to close before it's killed. Keep it under the container's stop timeout. |
| `BROWSERPIER_PROFILE_DIR` | `/profile` | |
| `BROWSERPIER_CONTROL_SOCKET` | `/run/browserpier/control.sock` | |

The container exits 0 only after a graceful stop: Chromium closed through CDP's `Browser.close` in time, so the profile is safely on disk. It exits 1 if Chromium crashed, failed to start, had to be killed, or had to be stopped with SIGTERM because CDP didn't answer (which can lose the last few seconds of cookie writes); the last log line says which.

## Control socket

HTTP over the Unix socket:

| Path | |
|---|---|
| `GET /healthz` | `ok` once Chromium answers on CDP. The socket only appears once it does. |
| `/cdp/...` | CDP over HTTP and WebSocket, relayed to Chromium's loopback port (`/cdp/json/version`, `/cdp/devtools/browser/...`). URLs in CDP's JSON still say `ws://127.0.0.1:9222`; rewrite them. |
| `GET /stream` | The display as an H.264 Annex-B byte stream (constrained baseline, 30 fps, keyframe every 2 s), one reader at a time (409 otherwise). The encoder runs only while it's read. |

## Seccomp profile

`seccomp.json` is Docker's default profile with `clone` and `unshare` allowed. Regenerate it with `go generate ./images/chromium` after bumping the pinned commit in `gen_seccomp.go`.
