---
title: Hand a browser to a person
description: When an agent reaches a step it can't do, such as a 2FA code or a CAPTCHA, it asks for a handoff, sends the person a one-time link and waits for them to hand back.
---

When your agent reaches a step only a person can do, such as a sign-in, a 2FA code or a CAPTCHA, it asks for a handoff. BrowserPier gives it a one-time link. The person opens it, does the step in the live view and hands the browser back. Meanwhile the agent is paused, then carries on.

**Before you start:**

- The agent has a running session, with its ID in `$SESSION`. See [Start or attach to a browser](/docs/reference/sessions/#start-or-attach-to-a-browser).
- A handoff PIN is set in **Settings**. Profiles ask for it by default, and a handoff that needs it is refused while none is set.
- You have a way to reach the person: a chat, Telegram, Slack. BrowserPier sends no notifications of its own.

## 1. Ask for a handoff

Say why, with a `reason`, and what the person should do, with a `detail`. The person sees both.

```sh
curl -s -X POST "https://pier.example.com/api/sessions/$SESSION/handoff" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"reason": "sensitive_input", "detail": "Enter the 2FA code GitHub sent to your phone"}'
```

```json
{
  "id": "h_mfrgg2lbnfxgk4tf",
  "session_id": "3f9c2a7d1e4b6c8a0d2f4e61",
  "profile": "github",
  "status": "pending",
  "reason": "sensitive_input",
  "policy": "pin",
  "origin": "https://github.com",
  "url": "https://pier.example.com/h/…",
  "expires_at": "2026-11-17T16:45:00Z",
  "expires_in": 900,
  "message": "Send the URL to the person through your own channel, then wait on GET /api/handoffs/{id}?wait=900. You are paused until they hand back."
}
```

Keep its `id` in `$HANDOFF` for the next steps. `reason` is one of:

| `reason` | Use it for |
|---|---|
| `sensitive_input` | A sign-in, a 2FA code, a payment |
| `captcha` | A CAPTCHA or a bot check |
| `file_upload` | Choosing a file from the person's device |
| `review` | Checking something before the agent goes on |
| `manual_step` | Anything else only a person can do |

## 2. Send the link to the person

Send `url` through your own channel, with the detail. The link:

- works once, in the first browser that opens it,
- lasts 15 minutes,
- gives control of this one browser only,
- asks for the handoff PIN, or the admin password, as the profile's policy says.

:::caution
The link opens a browser that's signed in to the person's accounts. Send it only to them, never to a shared channel.
:::

## 3. Wait for the hand-back

Long-poll the handoff. Each call waits up to `wait` seconds (at most 900) and returns as soon as the status changes.

```sh
while :; do
  status=$(curl -s "https://pier.example.com/api/handoffs/$HANDOFF?wait=900" \
    -H "Authorization: Bearer $PIER_TOKEN" | jq -r .status)
  [ "$status" != pending ] && break
done
echo "$status"
```

While the handoff is open, the agent can still take screenshots, but its clicks, typing and page actions answer `409`, and its CDP connections are closed. The person has the browser.

## 4. Carry on, or stop

| `status` | What happened | What the agent does |
|---|---|---|
| `done` | The person tapped **Done, hand back**. | Re-read the page, then carry on. |
| `cant` | The person tapped **Can't do this**. `note` may say why. | Stop the task and report back. |
| `expired` | The 15 minutes ran out before the person handed back. | Re-read the page, then ask again or report back. |
| `cancelled` | The browser stopped, five wrong PINs were entered, or the agent withdrew the request. `note` says which. | Re-read the page before acting. |

In every case the page may have changed, so read it again before the next action. The response's `message` says the same, for agents that read it.

## Hand back by itself when the page gets there

People often stop once they see the signed-in page, without tapping **Done**. If the agent knows where the step ends, it can say so with `until_url`. The handoff then ends as `done` when the tab in front navigates to an address containing that text. BrowserPier starts watching once the person has opened the link.

```sh
curl -s -X POST "https://pier.example.com/api/sessions/$SESSION/handoff" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"reason": "sensitive_input", "detail": "Sign in to GitHub", "until_url": "github.com/dashboard"}'
```

- It matches the origin and path only, never the query or fragment. Case counts.
- It must be 4 to 200 characters, without `?` or `#`.
- When it ends this way, the response has `"until_url_reached": true` and the note `the page reached a matching address`.
- The person sees that the handoff will end by itself, and can still tap **Done** or **Can't do this**.

## Ask for a stricter check

Each profile has a handoff policy. An agent can raise it for one handoff with `require`, but never lower it.

| `require` | The person needs |
|---|---|
| `link` | The link only |
| `pin` | The link and the handoff PIN |
| `signin` | The link and the admin password |

For email, banking or payments, ask for `signin`:

```sh
curl -s -X POST "https://pier.example.com/api/sessions/$SESSION/handoff" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"reason": "sensitive_input", "detail": "Approve the payment", "require": "signin"}'
```

## Take the request back

If the agent no longer needs the person, it withdraws the handoff. The link stops working and the agent has the browser back. This is refused once the person has opened the link.

```sh
curl -s -X DELETE "https://pier.example.com/api/handoffs/$HANDOFF" \
  -H "Authorization: Bearer $PIER_TOKEN" \
  -d '{"note": "found another way"}'
```

## Over MCP

The MCP tools do the same steps:

- `request_handoff` opens the handoff. Clients that can show links to their user, such as Claude, show it directly, and the call waits for the hand-back, up to 4 minutes at a time. Other clients get the link to pass on.
- `wait_for_handoff` waits again while the handoff is pending.
- `cancel_handoff` withdraws it.

## If something goes wrong

| Status | `error` | What to do |
|---|---|---|
| 409 | `no handoff PIN is set: ask the person to set one in Settings, or use require=signin` | Set a PIN in **Settings**, or ask for `"require": "signin"`. |
| 409 | `a handoff is already open for this browser: …` | Wait on the open one, or withdraw it. The message gives its ID. |
| 409 | `the session isn't running` | Start the session again, then ask. |
| 400 | `reason: want sensitive_input, captcha, file_upload, review or manual_step` | Use one of these reasons. |
| 409 | `a person has opened this link; wait for them to hand back` | Withdrawing is too late; wait instead. |

## Related

- [Handoffs API](/docs/reference/handoffs/): every field and error.
- [Sessions API](/docs/reference/sessions/): starting the browser the handoff is for.
