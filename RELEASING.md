# Releasing

Versions follow semver; 0.x releases may still change the API.

1. **Changelog.** On `main`, add a `## X.Y.Z` section at the top of `CHANGELOG.md`: what changed for people running BrowserPier and for agents. CI uses this section as the release notes.
2. **Browser definition.** If anything about how browser containers are started changed since the last release (flags, environment, mounts, entrypoint behaviour), bump `docker.Spec` in `internal/docker/docker.go`, so an update stops the old browsers instead of picking them up. Say so in the changelog's **Before you update** line, and say to restart any browser still running on the old image. The release with the file chooser portal (PROJ-152, PROJ-181; definition 7) must say it: a browser left on the old image keeps Chromium's GTK file dialog, which can hand a page any file in the container (PROJ-183), until it restarts.
3. **Check the files.** `deploy/release-files.sh X.Y.Z /tmp/dist` must succeed: it pins the install files and pulls the notes out of the changelog.
4. **Tag:** `git tag -a vX.Y.Z -m "BrowserPier X.Y.Z" && git push origin vX.Y.Z`. CI then:
   - runs the tests
   - publishes `ghcr.io/oni-giri/browser-pier` and `ghcr.io/oni-giri/browser-pier-chromium` as `X.Y.Z`, `X.Y` and `latest`, for amd64 and arm64
   - makes the GitHub release, with `docker-compose.yml`, `Caddyfile` and `env.example` pinned to `X.Y.Z`
5. **Check:**
   - the release page has its three files
   - both images pull without signing in: `docker pull ghcr.io/oni-giri/browser-pier:X.Y.Z` and `docker pull ghcr.io/oni-giri/browser-pier-chromium:X.Y.Z`
   - `docker run --rm ghcr.io/oni-giri/browser-pier:X.Y.Z -version` prints `vX.Y.Z`
6. **Umbrel.** In `umbrel/yakitori-browserpier/`:
   - **Pin the image:** in `docker-compose.yml`, set it to `ghcr.io/oni-giri/browser-pier:X.Y.Z@sha256:<digest>`. The digest is the multi-arch index's, from `docker buildx imagetools inspect ghcr.io/oni-giri/browser-pier:X.Y.Z`.
   - **Pin the browser image:** set `BROWSERPIER_BROWSER_IMAGE` the same way, with `browser-pier-chromium`'s digest.
   - **Version:** set `version:` in `umbrel-app.yml` to `X.Y.Z`, and fill in `releaseNotes`.
   - **Publish:** commit to `main`, then copy the folder into the app store repository, `Oni-giri/yakitoris-app-store`.
