# Contributing to BrowserPier

Read [SPEC.md](SPEC.md) first: it is the source of truth for behaviour, decisions and scope. For anything visual, follow [design/DESIGN.md](design/DESIGN.md) and the screens in `design/screens/`. [AGENTS.md](AGENTS.md) has the rules every change must keep (no ports on browser containers, no CDP in Visual mode, no fingerprint spoofing and so on), and it applies to people as much as to coding agents.

## Setting up a machine

You need four tools. CI runs on Ubuntu, so Linux is the reference platform; macOS works for building and the fast tests.

| Tool | Version | Why |
|---|---|---|
| [Go](https://go.dev/dl/) | the `go` line in `go.mod` | builds and tests the orchestrator and the browser image's entrypoint |
| [golangci-lint](https://golangci-lint.run) | v2.15.0, as in `.github/workflows/ci.yml` | `make lint` |
| Docker (or rootless Podman, below) | a recent Engine; Docker Desktop on macOS | the browser image and the `docker`-tagged tests |
| [GitHub CLI](https://cli.github.com) (`gh`) | any recent | pull requests and CI status |

Any Go from 1.21 on is enough to start: with the default `GOTOOLCHAIN=auto`, `go` downloads the exact version `go.mod` asks for the first time it runs here.

**Debian or Ubuntu:**

```sh
sudo apt-get install -y git make curl ca-certificates
# Go: the tarball from https://go.dev/dl/, then
sudo tar -C /usr/local -xzf go*.linux-*.tar.gz && export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$(go env GOPATH)/bin" v2.15.0
# Docker Engine: https://docs.docker.com/engine/install/, then let your user reach it
sudo usermod -aG docker "$USER"   # log out and back in
# gh: https://github.com/cli/cli/blob/trunk/docs/install_linux.md
```

**macOS (Homebrew):**

```sh
brew install go gh
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$(go env GOPATH)/bin" v2.15.0
brew install --cask docker   # Docker Desktop; start it once
export PATH=$PATH:$(go env GOPATH)/bin
```

**Then check it all works,** from the repository's root:

```sh
go version && golangci-lint version && docker info --format '{{.ServerVersion}}' && gh auth status
make lint test build   # fast: no Docker needed
make test-docker       # builds the browser image and runs the Docker tests (several minutes)
```

`gh auth login` signs the GitHub CLI in if `gh auth status` says it isn't. Coding agents start with [AGENTS.md](AGENTS.md); work is tracked in Jira, which an agent reaches through the Atlassian MCP connector.

## Building and testing

```sh
make build        # bin/browserpier
make test         # fast tests, no Docker needed
make test-docker  # builds the browser image, then also runs the tests tagged `//go:build docker`
make lint
make image        # the orchestrator image, ghcr.io/oni-giri/browser-pier
```

Tests that need Docker sit behind the `docker` build tag, so `go test ./...` stays fast. `BROWSERPIER_TEST_RECORD=1` runs the API package's Docker tests with every session recorded (PROJ-23), to check recording changes nothing a page or an agent sees; `BROWSERPIER_MEASURE=1` runs `TestMeasureRecordingCost` in `internal/sessions`, which logs the recording pipeline's CPU, memory and bytes per hour (`BROWSERPIER_MEASURE_SECONDS`, default 60, per scene). Before calling a change done, run it for real: start a browser, sign in to a test site, stop it, start it again and check that the login survived.

After a change to the browser image, set `BROWSERPIER_TEST_OLD_IMAGE` to the last released one (`ghcr.io/oni-giri/browser-pier-chromium:0.2.1`, say): `TestLoginSurvivesImageUpgrade` then checks a profile it made keeps its login on the new image. Measurements that aren't checks, such as `TestMeasureWebGLCost` (what WebGL's renderer costs, PROJ-148) and `TestMeasureWindowAtLoad`, also need the `measure` tag: `go test -tags 'docker measure' -run TestMeasureWebGLCost -v ./images/chromium/`.

### On rootless Podman

Start Podman's Docker API and point the tests at it, with a `docker` → `podman` shim on your `PATH`:

```sh
podman system service --time=0 unix://$XDG_RUNTIME_DIR/podman/podman.sock &
DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock BROWSERPIER_USERNS_MODE=keep-id \
  BROWSERPIER_TEST_SITE_HOST=host.docker.internal make test-docker
```

With Podman 5, that passes only after two changes to the host.

**Where the test sites are.** Podman 5 maps `host.docker.internal` to the link-local `169.254.1.2`, which the browser's egress filter always refuses, so every test page fails with `chrome-error://chromewebdata/`. Point it at another address of this machine in `~/.config/containers/containers.conf`:

```toml
[containers]
host_containers_internal_ip = "100.64.0.7"
```

Not the address on your default route: pasta copies that one into the containers' network, where it means the container itself. A second interface's address works, such as a Tailscale one. A running `podman system service` keeps the setting it started with, so restart it after the edit. `podman run --rm alpine getent hosts host.docker.internal` shows the address containers get. Setting `BROWSERPIER_TEST_SITE_HOST` to that address instead works too.

**Stopping the last container** (Ubuntu). When the last container on the rootless network stops, Podman kills `pasta`, and Ubuntu's AppArmor profile for `pasta` refuses the signal, so the stop returns `rootless netns: kill network process: permission denied`. The container has stopped, so BrowserPier only logs a warning, but the host should still be fixed: add this line to the profile in `/etc/apparmor.d/usr.bin.pasta`, after `include <abstractions/pasta>`,

```
  signal (receive) peer=podman,
```

then reload it with `sudo apparmor_parser -r /etc/apparmor.d/usr.bin.pasta`. The file comes with the `passt` package, so an upgrade may ask about the edit or undo it; check the line is still there afterwards.

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

### Changing what a container gets

Every container BrowserPier creates passes the create guard (`internal/docker/guard.go`) right before Docker sees it. The guard states, per kind of container (`browser`, `sandbox-check`), the value of every field BrowserPier sets, and refuses any other field that is set at all. It restates the builders' values on purpose instead of calling them, so don't make it derive anything from `browserConfig` or a `BrowserSpec`. To give a container something new (a device, a mount, a network, a limit, an environment variable for the entrypoint):

1. Allow it in the guard first: name the field in `guardNamed` and check its exact value for the kinds that get it (for an environment variable, add its name to `guardBrowserEnv`).
2. Then change the builder (`browserConfig`, `browserCreateOptions` or `sandboxCheckOptions`).
3. Then the tests: a row in `TestGuardRefusesEachWidening` for each way the new value could be wrong, and the golden bodies in `internal/docker/testdata` (`go test ./internal/docker -run TestCreateBodiesAreUnchanged -update-golden`), whose diff shows the change.

A new kind of container is a new `createKind` and a new case in `checkCreate`, written by hand. A new Docker operation (an exec, a network, a copy) is a deliberate edit of the list in `TestDockerOperationsAreAClosedSet`. Only `internal/docker` may import the Docker SDK.

CI lints, tests and builds every push and pull request. It publishes both images to ghcr.io from `main` (tag `edge`) and from `v*` tags, which also make the GitHub release. See [RELEASING.md](RELEASING.md).
