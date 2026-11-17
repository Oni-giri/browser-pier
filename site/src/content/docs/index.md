---
title: BrowserPier docs
description: What BrowserPier is, and where to find how to install it, connect an agent and call its API.
---

BrowserPier runs one real Chromium per profile (`github`, `google`, …) on your own server. You sign in once and the login stays. Agents drive the browsers over MCP, REST or CDP, and when a step needs a person, such as a 2FA code, the agent sends you a one-time link and waits.

## Where to start

- **Ask a person to take over a browser:** [Hand a browser to a person](/docs/guides/handoff/).
- **Start, list and stop browsers from code:** the [Sessions API](/docs/reference/sessions/).
- **Wait for a person, or take a request back:** the [Handoffs API](/docs/reference/handoffs/).

These docs are new, and the rest is still in the repository:

- **Installing on a server:** [deploy/README.md](https://github.com/Oni-giri/browser-pier/blob/main/deploy/README.md) covers Docker Compose, Tailscale or HTTPS, the firewall, every setting, updates and backups.
- **Connecting an agent:** the [README](https://github.com/Oni-giri/browser-pier#connect-your-agent) has the MCP setup for Claude Code and Codex, and Playwright and REST examples.
- **Every endpoint and how it's built:** [SPEC.md](https://github.com/Oni-giri/browser-pier/blob/main/SPEC.md), the design and the decisions behind it.

## Conventions in these docs

- `https://pier.example.com` stands for your BrowserPier's address.
- Examples read the agent token from the `PIER_TOKEN` environment variable. Create a token on the **Agents** page and set it where the agent runs:

  ```sh
  export PIER_TOKEN=bp_live_…
  ```

- Every page is also plain Markdown: add `.md` to its address. [llms.txt](/docs/llms.txt) lists them for agents.
