# BrowserPier

Self-hosted, persistent browsers for AI agents.

Agents start a browser when they need one. It keeps its cookies and logins, and it stops afterwards so an idle profile uses no RAM. When an agent hits a login, a 2FA prompt or a captcha, it sends you a one-time **handoff link**: you open it (on your phone, usually), do the step and hand control back.

- **Two ways to drive:**
  - **API mode** over CDP, for Playwright, browser-use and similar.
  - **Visual mode** with screenshots plus real OS-level mouse and keyboard, for sites with heavy anti-bot measures.
- **Persistent profiles:** one real, stable Chromium per profile (`google`, `linkedin`…), with logins that survive restarts and updates.
- **Live view in any browser** over WebRTC, with no VNC client. You can watch the agent or take over.
- **Web UI, REST API and MCP endpoint.**
- **Runs anywhere with Docker:** a generic docker-compose for a VPS or home server, plus an Umbrel app.

## Status

🚧 Design and spec are done; the code hasn't been started yet.

- Full spec: [SPEC.md](SPEC.md)
- Task tracking: Jira epic [PROJ-1](https://geistlogic.atlassian.net/browse/PROJ-1). Tasks are labelled `v1`, `v1.1` or `later`.

## How it works

```
 agents ──REST / MCP / CDP──▶  orchestrator (Go, always on)  ◀──WebRTC──  you (UI, live view, handoff)
                                   │  Docker SDK
                                   ▼
                      one browser container per running session
                      (headful Chromium + Xvfb + xdotool + H.264 stream)
                                   │
                      one persistent folder per profile
```

## Development

Needs Go (version in `go.mod`), [golangci-lint](https://golangci-lint.run) v2 and Docker or rootless Podman.

```sh
make build        # bin/browserpier
make test         # fast tests, no Docker needed
make test-docker  # also runs tests tagged `//go:build docker`
make lint
make image        # orchestrator image, ghcr.io/oni-giri/browser-pier
```

Configuration is through `BROWSERPIER_*` environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `BROWSERPIER_LISTEN` | `:7437` | Address the HTTP server binds to |
| `BROWSERPIER_DATA_DIR` | `./data` (`/data` in the image) | Database and one folder per profile |
| `BROWSERPIER_HOST_DATA_DIR` | detected | The data dir's path on the Docker host, for bind mounts into browser containers. In a container BrowserPier reads it from its own mounts; set it only if that fails. |
| `BROWSERPIER_BROWSER_IMAGE` | the image built with this version (`edge` for dev builds) | Browser image. BrowserPier starts containers from this image only. |
| `BROWSERPIER_USERNS_MODE` | none | `keep-id` under rootless Podman, so browsers write profile files as your user |

BrowserPier talks to Docker through the usual `DOCKER_HOST`. The data dir must be on a volume or bind mount when BrowserPier runs in a container, and it needs the Docker socket.

To run the Docker tests on rootless Podman, start its Docker API and point the tests at it:

```sh
podman system service --time=0 unix://$XDG_RUNTIME_DIR/podman/podman.sock &
DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock BROWSERPIER_USERNS_MODE=keep-id \
  BROWSERPIER_TEST_SITE_HOST=host.docker.internal make test-docker   # with a `docker` → `podman` shim on PATH
```

CI lints, tests and builds every push and pull request, and publishes the orchestrator image to ghcr.io from `main` (tag `edge`) and from `v*` tags.

## Security, in one paragraph

A BrowserPier profile holds live logged-in sessions. Anyone with CDP access to a profile effectively controls those accounts. Keep BrowserPier private (Tailscale or a VPN) when you can; if it's public, HTTPS is required. Give each agent its own token scoped to the profiles it needs.

## License

TBD.
