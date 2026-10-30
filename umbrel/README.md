# Umbrel app

`yakitori-browserpier/` is the BrowserPier package for the author's Umbrel community app store, [Oni-giri/yakitoris-app-store](https://github.com/Oni-giri/yakitoris-app-store) (store ID `yakitori`). It's kept here, next to the code it ships, and copied into the store to publish. See SPEC.md §12 for the decisions behind it.

## What's in it

- **`umbrel-app.yml`:** the listing. The app password (`deterministicPassword: true`) is BrowserPier's admin password. Umbrel opens the app on port 7436.
- **`docker-compose.yml`:** BrowserPier as service `web`, behind Umbrel's proxy.
  - Agent and handoff paths skip Umbrel's login: `/api`, `/cdp`, `/mcp`, `/h`, `/static`.
  - It mounts the Docker socket to start browsers, which is why this app can't go in Umbrel's official store.
  - Live views use port 7438, UDP and TCP, published directly.
- **`exports.sh`:** read on the host when the app starts: the host's LAN and Tailscale addresses, for live viewers, and the Docker socket's group.
- **`hooks/pre-stop`:** stops this BrowserPier's browsers gracefully whenever Umbrel stops, restarts, updates or uninstalls the app.
- **`data/`:** the app's data folder: the database and one folder per profile.

## Publishing

1. Pin the image. `docker-compose.yml` uses `edge` until the first release (PROJ-28). Then set `image:` to `ghcr.io/oni-giri/browser-pier:<version>@sha256:<digest>`, with the multi-arch index digest (`docker buildx imagetools inspect`), and set `version:` in `umbrel-app.yml` to match.
2. Copy `yakitori-browserpier/` into the root of the store repository and push.
3. On the Umbrel: App Store → ⋯ → Community App Stores → add `https://github.com/Oni-giri/yakitoris-app-store`, then install BrowserPier.

## Testing an install

- **Sign-in:** the app opens on Umbrel's login, then BrowserPier's: sign in with the password under BrowserPier → Info.
- **Live views:** a browser's live view plays over the LAN (`http://umbrel.local:7436`) and over Tailscale (the Umbrel's 100.x address).
- **Agents:** a token from the Agents page works on `/api`, `/cdp` and `/mcp` without an Umbrel login. A handoff link opens on a phone.
- **Stop and start:** a login in a browser survives stopping and starting the app in Umbrel, and no browser container is left running while it's stopped (`docker ps --filter label=io.browserpier.managed`).
