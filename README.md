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
                      one persistent volume per profile
```

## Security, in one paragraph

A BrowserPier profile holds live logged-in sessions. Anyone with CDP access to a profile effectively controls those accounts. Keep BrowserPier private (Tailscale or a VPN) when you can; if it's public, HTTPS is required. Give each agent its own token scoped to the profiles it needs.

## License

TBD.
