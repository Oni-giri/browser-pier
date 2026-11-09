# Contributing to BrowserPier

Read [SPEC.md](SPEC.md) first: it is the source of truth for behaviour, decisions and scope. For anything visual, follow [design/DESIGN.md](design/DESIGN.md) and the screens in `design/screens/`. [CLAUDE.md](CLAUDE.md) has the rules every change must keep (no ports on browser containers, no CDP in Visual mode, no fingerprint spoofing and so on), and it applies to people as much as to coding agents.

## Building and testing

You need Go (the version in `go.mod`), [golangci-lint](https://golangci-lint.run) v2, and Docker or rootless Podman.

```sh
make build        # bin/browserpier
make test         # fast tests, no Docker needed
make test-docker  # builds the browser image, then also runs the tests tagged `//go:build docker`
make lint
make image        # the orchestrator image, ghcr.io/oni-giri/browser-pier
```

Tests that need Docker sit behind the `docker` build tag, so `go test ./...` stays fast. Before calling a change done, run it for real: start a browser, sign in to a test site, stop it, start it again and check that the login survived.

### On rootless Podman

Start Podman's Docker API and point the tests at it, with a `docker` → `podman` shim on your `PATH`:

```sh
podman system service --time=0 unix://$XDG_RUNTIME_DIR/podman/podman.sock &
DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock BROWSERPIER_USERNS_MODE=keep-id \
  BROWSERPIER_TEST_SITE_HOST=host.docker.internal make test-docker
```

## Running it locally

```sh
make build
BROWSERPIER_LISTEN=127.0.0.1:7437 BROWSERPIER_DATA_DIR=./data BROWSERPIER_ADMIN_PASSWORD=change-me-please ./bin/browserpier
```

It listens on `127.0.0.1:7437`, so only your machine reaches it with that password (the default, `:7437`, is every interface). It keeps its database and profiles in `./data` (`BROWSERPIER_DATA_DIR`). It reaches Docker through the usual `DOCKER_HOST`. Every other setting is a `BROWSERPIER_*` environment variable, listed in [deploy/README.md](deploy/README.md#settings).

To try the API:

```sh
TOKEN=$(BROWSERPIER_DATA_DIR=./data ./bin/browserpier token create me | head -1)
curl -H "Authorization: Bearer $TOKEN" -X POST localhost:7437/api/profiles -d '{"name": "demo", "default_mode": "api"}'
curl -H "Authorization: Bearer $TOKEN" -X POST localhost:7437/api/sessions -d '{"profile": "demo"}'
# then, from Playwright: chromium.connectOverCDP("http://localhost:7437/cdp/demo", {headers: {Authorization: `Bearer ${TOKEN}`}})
curl -H "Authorization: Bearer $TOKEN" -X DELETE localhost:7437/api/sessions/<id>
```

## Layout

- `cmd/browserpier/`: the orchestrator's entry point
- `internal/`: its packages (`docker`, `sessions`, `profiles`, `auth`, `api`, `webrtc`, `ui`, `handoff`, `mcp`, `proxy`, `files`…)
- `images/chromium/`: the browser container image, its entrypoint and stream sidecar
- `deploy/`: the generic docker-compose, the Caddy config and the install guide
- `umbrel/`: the Umbrel app

## Changes

- Small, focused commits, each naming its task, e.g. `PROJ-5: session lifecycle state machine`.
- Write tests alongside the code.
- If the code and the spec disagree, raise it rather than diverging, and update SPEC.md when a decision changes.

CI lints, tests and builds every push and pull request. It publishes both images to ghcr.io from `main` (tag `edge`) and from `v*` tags, which also make the GitHub release. See [RELEASING.md](RELEASING.md).
