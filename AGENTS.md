# AGENTS.md: working on BrowserPier

- `SPEC.md` is the source of truth for behaviour, decisions and scope. If the code and the spec disagree, raise it rather than diverging, and update the spec when a decision changes.
- For anything visual, follow `design/DESIGN.md` and the screens in `design/screens/`.
- `CONTRIBUTING.md` covers building, testing, running locally and the repo layout.
- For the docs site (`site/`), follow `site/STYLE.md`. A change to behaviour that the docs describe updates them in the same PR.

## Task tracking

Work is tracked in Jira project **PROJ** on `geistlogic.atlassian.net`, under epic **PROJ-1**. The board is shared with other projects, so always filter by the `browserpier` label. Each task also has one of the labels `v1.1` or `later`; `optional` marks work that is nice to have and may never be done.

If the Atlassian MCP is connected:
- Move a task to **In Progress** before starting it.
- Comment on notable decisions or blockers.
- When it's done, comment with a short summary (what changed, how to test it, any follow-ups) and move it to **Done**.
- Open a new task for follow-up work you find (same labels, under PROJ-1) instead of widening the current one.

## Rules every change must keep

- Browser containers publish **no ports**. CDP, the stream and the visual endpoints go only through the orchestrator, behind token checks.
- **In Visual mode, no CDP client stays attached to the page.** Input goes through xdotool, and the stream comes from the X display.
- **No fingerprint spoofing.** Each profile keeps one real, stable browser.
- Chromium keeps its sandbox. Never use `--no-sandbox`.
- Chromium runs with `--password-store=basic` and is closed **gracefully** before its container stops, so logins aren't lost.
- One running session per profile. A second request attaches to the existing one.
- Agents can **raise** a handoff's security level but never lower it. Handoff links are single-use, last 15 minutes and give control of one browser only.
- Every piece of page-derived text returned to an agent goes through the untrusted-content fence (`internal/fence`).
- Tokens, the PIN and the admin password are hashed at rest and never logged.
- The orchestrator only starts containers from BrowserPier's own browser image.

## Conventions

- Branches are named `PROJ-<n>-<slug>`. Commits are small and start with the ticket key, e.g. `PROJ-48: every colour is a light-dark token`.
- Only the maintainer signs. Commits, pull requests and comments carry no AI credit: no `Co-Authored-By: Claude …` trailer, no "Generated with Claude Code" line, nothing like them.
- Write tests alongside the code. Tests that need Docker go behind the `docker` build tag, so `make test` stays fast.
- Docker tests are heavy, so watch the RAM. Each test browser takes about 300 MB, `-race` multiplies the test binary's memory several times, and running out can make the kernel kill the whole machine's session, agents included.
  - Check free memory (`free -m`) before a Docker run. On a small machine, run one package at a time, not the full `make test-docker`.
  - Several agents sharing a machine run Docker tests one after another, never in parallel. Share a lock for it, e.g. `flock /tmp/docker-tests.lock <command>`.
  - Run big test commands under a memory cap, so a runaway test is killed alone. For example, `systemd-run --user --scope -p MemoryMax=6G -- go test …`.
  - Keep stress probes bounded. Start small and scale up, and don't run the largest shapes under `-race`.
  - A killed test run leaves its browser containers running. Afterwards, check `docker ps -a` for leftover `browserpier-*` and `bp-test-*` containers and remove them before the next run.
- Before calling a task done, run it for real: start a browser, sign in to a test site, stop it, start it again and check the login survived.
- Prefer the standard library and a few well-known dependencies.
- Configuration is through `BROWSERPIER_*` environment variables, documented in `deploy/README.md`.
- User-facing changes go in `CHANGELOG.md` at release time (see `RELEASING.md`).
- Code reused from Steel (Apache 2.0) keeps its attribution, with an entry in `NOTICE`.
