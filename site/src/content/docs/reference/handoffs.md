---
title: Handoffs API
description: Ask a person to take over a running browser, wait for them to hand back, or withdraw the request. Fields, statuses and errors.
---

A handoff asks a person to take over a running browser for a step the agent can't do. It gives a one-time link that lasts 15 minutes. For the whole flow, with examples, see [Hand a browser to a person](/docs/guides/handoff/).

Every request takes the agent token: `Authorization: Bearer $PIER_TOKEN`.

## Ask for a handoff

`POST /api/sessions/{id}/handoff`

Opens a handoff on a running session and returns its link. The agent is paused until the handoff ends: screenshots still work, but input and page actions answer `409` and CDP connections close.

### Request

| Field | Type | Required | Description |
|---|---|---|---|
| `reason` | string | yes | `sensitive_input`, `captcha`, `file_upload`, `review` or `manual_step`. |
| `detail` | string | no | What the person should do, such as "Enter the 2FA code". Up to 500 characters; longer is cut. |
| `require` | string | no | `link`, `pin` or `signin`. Raises the profile's policy for this handoff; a lower level is ignored. |
| `until_url` | string | no | Ends the handoff as `done` when the tab in front reaches an address containing this text. 4 to 200 characters, matched against the origin and path, without `?` or `#`. |

### Response

`201 Created`, with a [handoff](#the-handoff-object) that also has `url` and `expires_in`.

### Example

```sh
curl -s -X POST "https://pier.example.com/api/sessions/$SESSION/handoff" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"reason": "captcha", "detail": "Solve the CAPTCHA on the sign-in page"}'
```

```json
{
  "id": "h_mfrgg2lbnfxgk4tf",
  "session_id": "3f9c2a7d1e4b6c8a0d2f4e61",
  "profile": "github",
  "status": "pending",
  "reason": "captcha",
  "policy": "pin",
  "origin": "https://github.com",
  "url": "https://pier.example.com/h/…",
  "expires_at": "2026-11-17T16:45:00Z",
  "expires_in": 900,
  "message": "Send the URL to the person through your own channel, then wait on GET /api/handoffs/{id}?wait=900. You are paused until they hand back."
}
```

### Errors

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such session` | Start the session first. |
| 409 | `no handoff PIN is set: ask the person to set one in Settings, or use require=signin` | Ask the person to set a PIN, or use `"require": "signin"`. |
| 409 | `a handoff is already open for this browser: …` | Wait on the open one, or withdraw it. The message gives its ID. |
| 409 | `the session isn't running` | Start the session again. |
| 400 | `reason: want sensitive_input, captcha, file_upload, review or manual_step` | Use one of these. |
| 400 | `require: want link, pin or signin` | Use one of these, or leave `require` out. |
| 400 | `until_url: …` | The rest of the message says what's wrong with the pattern. |

## Wait for the hand-back

`GET /api/handoffs/{id}?wait=SECONDS`

Returns the [handoff](#the-handoff-object). With `wait`, it holds the request open until the status changes or `wait` seconds pass, whichever is first. `wait` is capped at 900. Without it, it answers at once.

```sh
curl -s "https://pier.example.com/api/handoffs/$HANDOFF?wait=900" \
  -H "Authorization: Bearer $PIER_TOKEN"
```

```json
{
  "id": "h_mfrgg2lbnfxgk4tf",
  "session_id": "3f9c2a7d1e4b6c8a0d2f4e61",
  "profile": "github",
  "status": "done",
  "reason": "captcha",
  "policy": "pin",
  "origin": "https://github.com",
  "expires_at": "2026-11-17T16:45:00Z",
  "note": "solved it",
  "message": "The person handed back. The page may have changed: re-read it before acting."
}
```

While `status` is `pending`, call it again.

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such handoff` | Check the ID. A token sees only handoffs on its profiles. |
| 400 | `wait: want seconds` | Give a whole number of seconds, 0 or more. |

## Withdraw a handoff

`DELETE /api/handoffs/{id}`

Takes back a request the agent no longer needs. The link stops working, the status becomes `cancelled` and the agent has the browser back. The body is optional.

| Field | Type | Required | Description |
|---|---|---|---|
| `note` | string | no | Why, for the activity log. |

```sh
curl -s -X DELETE "https://pier.example.com/api/handoffs/$HANDOFF" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"note": "found another way"}'
```

Returns the [handoff](#the-handoff-object), with `note` `the agent withdrew it: found another way`.

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such handoff` | Check the ID. |
| 409 | `a person has opened this link; wait for them to hand back` | Too late to withdraw: wait instead. |
| 400 | `want {"note": "..."} or nothing` | Send only `note`, or no body. |

## The handoff object

| Field | Description |
|---|---|
| `id` | The handoff's ID, `h_` and 16 characters. |
| `session_id` | The session it's for. |
| `profile` | The profile's name. |
| `status` | `pending`, `done`, `cant`, `cancelled` or `expired`. See below. |
| `reason` | The reason the agent gave. |
| `policy` | What the link asks for: `link`, `pin` or `signin`. The stricter of the profile's policy and `require`. |
| `origin` | The origin of the page in front when the handoff opened, such as `https://github.com`. Never the path. Left out when there was none. |
| `url` | The link for the person. Only in the response that created it. |
| `expires_at` | When the link stops working, in UTC. |
| `expires_in` | Seconds until then (900). Only in the response that created it. |
| `note` | The person's note, or why the handoff was cancelled. Left out when empty. |
| `until_url` | The agent's `until_url`, echoed. Left out when none. |
| `until_url_reached` | `true` when the handoff ended because the tab reached `until_url`. Left out otherwise. |
| `message` | What the agent should do next, in plain words. |

### Statuses

| `status` | Meaning | Next step |
|---|---|---|
| `pending` | Waiting for the person. | Wait again. |
| `done` | The person handed back, or the tab reached `until_url`. | Re-read the page, then carry on. |
| `cant` | The person tapped **Can't do this**. | Stop the task and report back. |
| `cancelled` | The browser stopped, five wrong PINs were entered, or the agent withdrew it. `note` says which. | Re-read the page before acting. |
| `expired` | The 15 minutes ran out. | Re-read the page, then ask again or report back. |

## Over MCP

| Tool | Does |
|---|---|
| `request_handoff` | Opens a handoff. Clients that can show links to their user show it, and the call waits up to 4 minutes for the hand-back. Other clients get the link to pass on. |
| `wait_for_handoff` | Waits again while the handoff is pending. |
| `cancel_handoff` | Withdraws it. |
