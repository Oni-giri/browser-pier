---
title: Sessions API
description: Start or attach to a profile's browser, list running browsers, get one, and stop it. A session is a running browser on one profile, and a profile has at most one.
---

A session is a running browser on one profile. A profile has at most one session, so two agents asking for the same profile share it.

Every request takes the agent token: `Authorization: Bearer $PIER_TOKEN`. A token sees only the profiles it's allowed; anything else answers `404`, as if it didn't exist.

## Start or attach to a browser

`POST /api/sessions`

Starts the profile's browser, or attaches to it if it's already running. A start takes a few seconds, and the call returns once the browser is up.

### Request

| Field | Type | Required | Description |
|---|---|---|---|
| `profile` | string | yes | The profile's name, such as `github`. |
| `mode` | `api` or `visual` | no | How the agent will drive the browser. Omit it to use the profile's default mode. |
| `block_ads` | boolean | no | `false` turns ad and tracker blocking off for this session, a running one included. It can't turn blocking on. |

### Response

`201 Created` when it started the browser, `200 OK` when it attached to a running one. The body is a [session](#the-session-object).

### Example

```sh
curl -s -X POST https://pier.example.com/api/sessions \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"profile": "github", "mode": "api"}'
```

```json
{
  "id": "3f9c2a7d1e4b6c8a0d2f4e61",
  "profile": "github",
  "mode": "api",
  "state": "running",
  "attached": false,
  "cdp_url": "wss://pier.example.com/cdp/github",
  "live_view_url": "https://pier.example.com/profiles/github",
  "block_ads": true,
  "created_at": "2026-11-17T16:30:00Z"
}
```

Keep the `id` for the other session calls:

```sh
SESSION=$(curl -s -X POST https://pier.example.com/api/sessions \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"profile": "github"}' | jq -r .id)
```

### Errors

| Status | `error` | What to do |
|---|---|---|
| 404 | `no profile named "github"` | Check the name, or ask the admin to allow the profile for this token. |
| 400 | `mode: want api or visual` | Use `api` or `visual`, or leave `mode` out. |
| 400 | `bad JSON: …` | Send a JSON object with only the fields above. |
| 409 | `the profile is being exported or imported; try again when that's done` | Retry in a minute. |
| 503 | `not enough memory for another browser: …` | Stop a browser you don't need. The message names yours. |
| 503 | `BrowserPier is shutting down` | Retry once it's back. |
| 502 | (a session, with `error` and `last_log`) | The browser didn't start. `error` and `last_log` say why. |

## List running browsers

`GET /api/sessions`

Lists the sessions of the profiles this token may use. A stopped profile has no session.

```sh
curl -s https://pier.example.com/api/sessions \
  -H "Authorization: Bearer $PIER_TOKEN"
```

```json
{
  "sessions": [
    {
      "id": "3f9c2a7d1e4b6c8a0d2f4e61",
      "profile": "github",
      "mode": "api",
      "state": "running",
      "attached": false,
      "cdp_url": "wss://pier.example.com/cdp/github",
      "live_view_url": "https://pier.example.com/profiles/github",
      "in_use_by": ["claude"],
      "block_ads": true,
      "created_at": "2026-11-17T16:30:00Z"
    }
  ]
}
```

`sessions` is an empty array when nothing is running.

## Get a session

`GET /api/sessions/{id}`

Returns one [session](#the-session-object). Use it to check `state`, for example after a handoff.

```sh
curl -s "https://pier.example.com/api/sessions/$SESSION" \
  -H "Authorization: Bearer $PIER_TOKEN"
```

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such session` | The browser has stopped, or this token can't see its profile. Start it again. |

## Stop a browser

`DELETE /api/sessions/{id}`

Closes the browser gracefully and keeps the profile, so its logins survive. Other agents attached to it lose it too, so stop a browser only when you know nobody needs it. An idle browser stops by itself after the profile's auto-stop time (15 minutes by default).

```sh
curl -s -X DELETE "https://pier.example.com/api/sessions/$SESSION" \
  -H "Authorization: Bearer $PIER_TOKEN"
```

Returns the [session](#the-session-object), with `state` `stopped`.

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such session` | It has already stopped. |
| 503 | `BrowserPier is shutting down` | Nothing: shutting down stops it too. |
| 502 | (the reason) | The browser didn't stop cleanly. Check the profile in the web UI. |

## The session object

| Field | Description |
|---|---|
| `id` | The session's ID, 24 hex characters. |
| `profile` | The profile's name. |
| `mode` | `api` or `visual`. |
| `state` | `starting`, `running`, `needs_help` (a handoff is open), `stopping`, `stopped` or `failed`. |
| `attached` | `true` when this call attached to a browser that was already running. Only set by `POST /api/sessions`. |
| `cdp_url` | The browser's CDP endpoint, through BrowserPier. Only in API mode, while the browser is up. Connect with the same `Authorization` header. |
| `live_view_url` | The profile's page in the web UI, where a person can watch or take control. It needs the admin sign-in, not the token. |
| `in_use_by` | The agents connected to the browser right now, by token name. Left out when none. |
| `error` | Why the browser failed or didn't start. Left out otherwise. |
| `last_log` | The browser container's last log line, marked as untrusted text. Only when there is one. |
| `block_ads` | Whether ad and tracker blocking is on for this session. |
| `ad_list_missing` | `true` when blocking was asked for but no list had been downloaded yet, so `block_ads` is `false`. Left out otherwise. |
| `created_at` | When the session started, in UTC. |
