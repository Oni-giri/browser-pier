# Umbrel app

`yakitori-browserpier/` is the BrowserPier package for the author's Umbrel community app store, [Oni-giri/yakitoris-app-store](https://github.com/Oni-giri/yakitoris-app-store) (store ID `yakitori`). It's kept here, next to the code it ships, and copied into the store to publish. See SPEC.md §12 for the decisions behind it.

## What's in it

- **`umbrel-app.yml`:** the listing. It needs umbrelOS 2.0 or later (`manifestVersion: 2.0`), whose proxy is the one BrowserPier trusts. The app password (`deterministicPassword: true`) is BrowserPier's admin password. Umbrel opens the app on port 7436.
- **`docker-compose.yml`:** BrowserPier as service `web`, behind Umbrel's proxy.
  - Agent and handoff paths skip Umbrel's login: `/api`, `/cdp`, `/mcp`, `/h`, `/static`, and the docs at `/docs`.
  - It mounts the Docker socket to start browsers, which is why this app can't go in Umbrel's official store.
  - Live views use port 7438, UDP and TCP, published directly.
- **`exports.sh`:** read on the host when the app starts: the host's LAN and Tailscale addresses, for live viewers, and the Docker socket's group.
- **Stopping:** with `BROWSERPIER_STOP_BROWSERS_ON_EXIT`, BrowserPier stops its browsers gracefully whenever Umbrel stops, restarts, updates or uninstalls the app. **`hooks/post-stop`** then stops any it missed, if it was killed, say.
- **AppArmor:** umbrelOS 2.0 runs AppArmor 4, where Docker's AppArmor profile refuses Chromium's sandbox with Docker 29.1 and earlier (umbrelOS 2.0 ships 28.5). **`hooks/pre-start`** loads BrowserPier's own profile with the host's `apparmor_parser` before every start of the app, after a reboot too, and says what it did in one line of umbreld's journal, starting `BrowserPier AppArmor:`. The hook carries the profile and its loader itself, as heredocs it writes to a temporary folder: Umbrel's app update copies only the files it knows (`docker-compose.yml`, `umbrel-app.yml`, `exports.sh`, `hooks/`, `icon.svg`), so an `apparmor/` folder next to it never arrives. They're copies of `images/chromium/apparmor/browserpier` and `load`: edit those, then run `go generate -run gen_umbrel_hook ./images/chromium` (a test fails until the copies match). If loading fails, browsers run under Docker's profile if Chromium's sandbox works there, otherwise without AppArmor, and BrowserPier's log says which and why. See "AppArmor" in `deploy/README.md`.
- **`data/`:** the app's data folder: the database and one folder per profile.
- **`icon.svg`:** the listing's icon, a copy of `internal/ui/static/icon.svg`. `umbrel-app.yml` loads it from the store repository, which is public; this one is private for now.

## Publishing

1. Pin the images to the release (see step 6 of `RELEASING.md`):
   - set `image:` to `ghcr.io/oni-giri/browser-pier:<version>@sha256:<digest>`, with the multi-arch index digest (`docker buildx imagetools inspect`)
   - set `BROWSERPIER_BROWSER_IMAGE` to `ghcr.io/oni-giri/browser-pier-chromium:<version>@sha256:<digest>`
   - set `version:` in `umbrel-app.yml` to match, and write `releaseNotes` as text: a link into this repository doesn't open while it's private
2. Copy `yakitori-browserpier/` into the root of the store repository and push.
3. On the Umbrel: App Store → ⋯ → Community App Stores → add `https://github.com/Oni-giri/yakitoris-app-store`, then install BrowserPier.

## Testing an install

- **Sign-in:** the app opens on Umbrel's login, then BrowserPier's: sign in with the password under BrowserPier → Info.
- **Live views:** a browser's live view plays over the LAN (`http://umbrel.local:7436`) and over Tailscale (the Umbrel's 100.x address).
- **Agents:** a token from the Agents page works on `/api`, `/cdp` and `/mcp` without an Umbrel login. A handoff link opens on a phone.
- **AppArmor:** umbreld's log (Umbrel's system logs) shows `BrowserPier AppArmor: loaded the browserpier profile` after `Executing hook: …/pre-start`, and BrowserPier's log says `browser containers run under BrowserPier's AppArmor profile`; `sudo aa-status | grep browserpier` lists it, and `docker inspect -f '{{.AppArmorProfile}}' browserpier-<profile>` says `browserpier` for a running browser. After a reboot, the same.
- **Stop and start:** a login in a browser survives stopping and starting the app in Umbrel, and no browser container is left running while it's stopped (`docker ps --filter label=io.browserpier.managed`).
