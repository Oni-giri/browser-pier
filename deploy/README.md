# Installing BrowserPier on a server

BrowserPier runs as one container. It starts a browser container per running profile through the host's Docker, from its own browser image. These steps are for a fresh Linux VPS or home server with Docker and the Compose plugin. For Umbrel, see `../umbrel/`.

**You need:**
- Docker 24 or newer with `docker compose`
- about 1–2 GB of RAM per browser you'll run at once
- a few GB of disk for profiles

## Choose how you'll reach it

1. **Tailscale or another VPN (recommended).** BrowserPier is reachable only on your tailnet: nothing but the live-view port faces the internet, and handoff links work from your phone as long as it's on the tailnet. Plain HTTP is fine there; handoff links accept it from private and Tailscale addresses.
2. **A domain, with Caddy** (`--profile https`). Caddy gets and renews a certificate. Use this to reach BrowserPier from anywhere, phones off your VPN included.
3. **Your own reverse proxy**, if the server already runs one. See [Behind your own proxy](#behind-your-own-proxy).

Over the public internet, use HTTPS: a handoff link opened over plain HTTP from outside your network asks for nothing and opens nothing.

## Install

```sh
umask 077   # .env and ./data hold secrets: yours only
mkdir browserpier && cd browserpier
release=https://github.com/Oni-giri/browser-pier/releases/latest/download
curl -fsSLO $release/docker-compose.yml
curl -fsSLO $release/Caddyfile
curl -fsSL  $release/env.example -o .env
install -d -m 700 data
```

While the repository is private, those downloads and the images need a GitHub sign-in: get the files with `gh release download --repo Oni-giri/browser-pier --pattern '*'` (rename `env.example` to `.env`), and run `docker login ghcr.io` with a token that can read packages.

These come from the latest release and run that release. The files on the `main` branch are for development and may need a newer image than the latest release.

Edit `.env`:
- **`PUID`, `PGID`:** your user (`id -u`, `id -g`). `./data` belongs to it, and the browsers run as it.
- **`DOCKER_GID`:** the Docker socket's group (`stat -c %g /var/run/docker.sock`).
- **`BROWSERPIER_PUBLIC_URL`:** how you'll open BrowserPier, e.g. `https://pier.example.com` or `http://100.101.102.103:7437`.
- **`BROWSERPIER_WEBRTC_IPS`:** the address live viewers reach this server at: its public IP, or its Tailscale IP.
- **With Caddy:** `BROWSERPIER_DOMAIN`, a DNS name already pointing at this server.
- **With Tailscale:** `BROWSERPIER_BIND`, the server's Tailscale address (`tailscale ip -4`), so the app listens there.

Then start it:

```sh
docker compose up -d                   # Tailscale, or your own proxy
docker compose --profile https up -d   # with Caddy
```

Compose pulls the browser image too: its `apparmor` service runs once and exits (see [AppArmor](#apparmor)).

## First run

Unless you set `BROWSERPIER_ADMIN_PASSWORD`, BrowserPier asks you to choose the admin password:
1. Get the one-time setup code from the log: `docker compose logs browserpier | grep setup_code`.
2. Open BrowserPier and enter the code and your password. The setup page then disappears.

The code keeps a stranger who reaches a fresh server first from claiming it.

Next:
- **Settings:** set a **handoff PIN**. Handoff links ask for it by default, and agents can't open one while it's unset.
- **Agents:** create a token per agent. Tokens can also be managed from the command line: `docker compose exec browserpier browserpier token create NAME`.

## Firewall

| Port | Open to | What |
|---|---|---|
| 80, 443 (TCP; 443 UDP too) | everyone | Caddy, if you use it |
| 7438 UDP and TCP | the people who watch or take over browsers | live views (WebRTC) |
| 7437 TCP | **nobody on the internet** | the app; Caddy or your proxy reach it locally, Tailscale on the tailnet |

Browser containers publish no ports. They're reached only through BrowserPier, behind its token checks.

## The Docker socket

BrowserPier starts browsers through `/var/run/docker.sock`, and **anything that can use the Docker socket controls the host**. A flaw in BrowserPier could therefore become a way into the server. What limits that:
- BrowserPier starts containers only from its own browser image. Browsers run without capabilities beyond what Chromium's sandbox needs, with no-new-privileges, seccomp, and no published ports.
- The orchestrator image is distroless and runs as your user, not root.

Still:
- Prefer a server or VM that runs nothing more sensitive.
- Keep BrowserPier behind Tailscale where you can.
- Keep it up to date.

## AppArmor

On Ubuntu and Debian, Docker confines containers with its AppArmor profile, `docker-default`. That profile has no rule for user namespaces, which Chromium's sandbox needs, and under AppArmor 4 without Ubuntu's own user namespace restriction (Debian 13 and later) it refuses them: Chromium aborts with `Check failed: … Permission denied (13)`. Ubuntu and Debian 12 aren't affected.

So BrowserPier has an AppArmor profile of its own, `browserpier`: Docker's, plus user namespaces. The compose file's **`apparmor` service** loads it. It runs once at each `docker compose up`, as root with only `CAP_MAC_ADMIN`, no network and a read-only root, and exits. Where the profile is needed, it loads it and installs it as `/etc/apparmor.d/browserpier`, so the host loads it again at boot. Anywhere else it says why it isn't needed and does nothing. On a host without AppArmor, Docker creates an empty `/etc/apparmor.d` for its mount. If loading fails, the service fails and BrowserPier doesn't start: `docker compose logs apparmor` says why.

At start, unless `BROWSERPIER_BROWSER_APPARMOR` names a profile, BrowserPier picks the browsers' AppArmor profile. Where Docker uses its stock profile, and the host has no Ubuntu restriction or is a remote `DOCKER_HOST`, it starts a short-lived container from its browser image, with the browsers' settings, to check where Chromium's sandbox works, in this order:
1. **`browserpier`**, if it's loaded.
2. **Docker's `docker-default`** (Debian 12, or a remote Ubuntu).
3. **None** (`unconfined`), with a warning in the log. The seccomp profile, dropped capabilities and no-new-privileges still apply. BrowserPier did this on Debian 13 before it had a profile of its own, so a server updated without the `apparmor` service keeps working.

If nothing works, BrowserPier logs an error: user namespaces are probably turned off on the host. On Ubuntu, and with a default profile you set for the daemon, browsers keep Docker's profile, unchecked. The log says what was chosen.

**To load the profile yourself** instead of through the service (for example with your own compose file), as root on the Docker host, from the release's browser image:

```sh
docker run --rm --entrypoint cat ghcr.io/oni-giri/browser-pier-chromium:<version> \
  /usr/share/browserpier/apparmor/browserpier > /etc/apparmor.d/browserpier
apparmor_parser -r /etc/apparmor.d/browserpier
```

Then restart BrowserPier so it checks again, or set `BROWSERPIER_BROWSER_APPARMOR=browserpier`. `aa-status | grep browserpier` shows it's loaded, and `docker inspect -f '{{.AppArmorProfile}}' <browser container>` shows a browser using it. To remove it: `apparmor_parser -R /etc/apparmor.d/browserpier && rm /etc/apparmor.d/browserpier`.

## Behind your own proxy

Proxy everything to `127.0.0.1:7437`, WebSockets included (CDP and the live view use them). Pass the real client address and scheme. Keep query strings out of your logs, error logs included: agents that can't set headers connect to `/cdp/…` with a `?ticket=`. A ticket is single-use and lasts a minute, but there's no reason to log it. The shipped `Caddyfile` shows how for Caddy.

Caddy: start from the shipped `Caddyfile`, with `reverse_proxy 127.0.0.1:7437`.

nginx:

```nginx
server {
    server_name pier.example.com;
    listen 443 ssl;
    # ssl_certificate …
    access_log off;
    client_max_body_size 8g;   # profile imports
    location / {
        proxy_pass http://127.0.0.1:7437;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 1h;     # long-polls and live views
        proxy_buffering off;       # exports stream as they're made
    }
}
```

BrowserPier believes `X-Forwarded-For` and `X-Forwarded-Proto` only from `BROWSERPIER_TRUSTED_PROXIES`. In the compose file that's Caddy's fixed address, `10.247.74.3`. With Docker, a proxy on the host reaches BrowserPier from the compose network's gateway: set `BROWSERPIER_TRUSTED_PROXIES=10.247.74.1` in `.env`. Under rootless Podman, every connection to a published port seems to come from the container's own address, so there's no telling your proxy from anyone else; use the bundled Caddy there.

## Settings

Set in `.env` and passed through by `docker-compose.yml`. Add any other variable from this list to its `environment:`. An empty value means unset.

Leave `BROWSERPIER_LISTEN` and `BROWSERPIER_WEBRTC_PORT` alone with this compose file: its port mappings and the Caddyfile expect 7437 and 7438. To serve the app on another host port, change only the left side of `7437:7437`. To move the live-view port, set `BROWSERPIER_WEBRTC_PORT` and change both sides of both `7438` mappings, since viewers are told the port BrowserPier listens on.

| Variable | Default | What |
|---|---|---|
| `BROWSERPIER_PUBLIC_URL` | the address used | How people and agents reach BrowserPier. An https URL makes cookies Secure. |
| `BROWSERPIER_WEBRTC_IPS` | every local address | Addresses advertised to live viewers. Pages can't open them, as they're this server's own. |
| `BROWSERPIER_WEBRTC_PORT` | 7438 | The live-view port, UDP with TCP as the fallback. |
| `BROWSERPIER_ADMIN_PASSWORD` | unset | The admin password. It wins over one changed in the UI. |
| `BROWSERPIER_RAM_LIMIT_PERCENT` | 70 | The share of host memory all browsers may use together. Unset: set in Settings. A new browser must also fit in what the host has free, other apps included. Past either, the longest-idle browser no agent is using is stopped to make room, or the start is refused. `0`: no limit, but the host is still checked. |
| `BROWSERPIER_FILES_LIMIT_MB` | 1024 | The cap on each profile's downloads and uploads. |
| `BROWSERPIER_TRUSTED_PROXIES` | none (Caddy's `10.247.74.3` in compose) | Proxies whose `X-Forwarded-*` headers are believed: addresses, CIDRs, or `private`. |
| `BROWSERPIER_NET` | `10.247.74` | The compose network's first three numbers (compose only). |
| `BROWSERPIER_VERSION` | the release the files came from | The image tag (compose only): a release such as `0.1.0` stays put, `latest` follows each release, `edge` follows the main branch. |
| `BROWSERPIER_COOKIE_SECURE` | auto | `always` or `never` to override. |
| `BROWSERPIER_GEOIP_URL` | `https://ifconfig.co/json` | How browsers find their exit location, to match time zone and language. `off` disables it. |
| `BROWSERPIER_LISTEN` | `:7437` | The app's address inside the container. |
| `BROWSERPIER_BROWSER_IMAGE` | this release's | The browser image. Only BrowserPier's own. |
| `BROWSERPIER_DATA_DIR` | `/data` | The database and one folder per profile, inside the container. Keep the `./data:/data` mount. |
| `BROWSERPIER_HOST_DATA_DIR` | detected | The data folder's path on the host, if detection fails. |
| `BROWSERPIER_USERNS_MODE` | unset | `keep-id` under rootless Podman. |
| `BROWSERPIER_BROWSER_APPARMOR` | unset | The browser containers' AppArmor profile. Unset, BrowserPier picks one: see [AppArmor](#apparmor). Set, that profile is used as it is, unchecked: `browserpier`, `unconfined`, or one of your own. A named profile that isn't loaded makes every browser fail to start, with an error saying so. |
| `BROWSERPIER_STOP_BROWSERS_ON_EXIT` | `false` | `true` stops every browser when BrowserPier stops. Otherwise they keep running through an update. |

## Updating and backups

Set `BROWSERPIER_VERSION` in `.env` to the new release (see the [releases](https://github.com/Oni-giri/browser-pier/releases) and `CHANGELOG.md`), or to `latest` to follow every release, then:

```sh
docker compose pull && docker compose up -d
```

Running browsers keep running through an update, and the new orchestrator picks them up. If the update changed how browsers are started, it stops them instead, the usual way with logins kept, and they start afresh next time.

`./data` holds everything: the database and one folder per profile, with its logins. Back it up like a password manager's vault: anyone with a copy can use those logins.

Copy it only while nothing writes to it, or the copy may not open:
1. In BrowserPier, stop every running browser. `docker compose stop` doesn't: browsers keep running without the orchestrator.
2. `docker compose stop`, so the database is closed.
3. Copy `./data`, keeping owners and modes (`sudo tar -cpzf backup.tgz data`).
4. `docker compose up -d`.

To move one profile, use **Export** on its ⋯ menu instead. That gives an encrypted archive, and works while the rest keep running.

## If something's wrong

- **"can't reach Docker":** `DOCKER_GID` doesn't match the socket's group.
- **"browser containers run without an AppArmor profile"** in the log: BrowserPier's AppArmor profile isn't loaded. Check `docker compose logs apparmor`, or load it by hand: see [AppArmor](#apparmor).
- **"service apparmor didn't complete successfully":** loading the AppArmor profile failed; `docker compose logs apparmor` says why.
- **"the data dir … isn't on a volume or bind mount":** keep the `./data:/data` mount.
- **A page on your LAN or this server won't load:** pages can't reach private addresses, so a page can't get to your router, the server's other services or a VPS's metadata. To let one profile reach them, tick "Let pages reach the local network" in its settings.
- **Live view never connects:** port 7438 isn't open (UDP, and TCP for the fallback), or `BROWSERPIER_WEBRTC_IPS` isn't the address the viewer reaches.
- **"Pool overlaps with other one on this address space":** `10.247.74.0/24` is taken on this host. In `.env`, set `BROWSERPIER_NET` to other first three numbers (say `10.247.75`) and `BROWSERPIER_TRUSTED_PROXIES` to match (`10.247.75.3`).
- **Handoff link says it needs HTTPS:** the phone opened it over plain HTTP from outside your network. Use Caddy, or open it on Tailscale.
