# Writing the BrowserPier docs

The docs at browserpier.com/docs are read by two kinds of reader: people setting BrowserPier up or wiring an agent to it, and coding agents doing the same. Both want the answer first, in a predictable place, with an example that works. This guide is how we give them that.

`SPEC.md` stays the source of truth. The docs say what to do and what to expect; the spec says why. If they disagree, the spec wins and the docs get fixed in the same PR.

## Every page has one job

Pick the type before writing. Don't mix them on one page.

| Type | The reader asks | The page is | Lives in |
|---|---|---|---|
| Tutorial | "Get me from nothing to working." | Numbered steps, each ending with what you should see. One path, no options. | `start/` |
| How-to | "How do I do X?" | A goal in the title, a short "before you start", then steps. No background. | `guides/` |
| Reference | "What exactly does Y take and return?" | The same template on every page, complete and dry. | `reference/` |
| Explanation | "Why does it work this way?" | Prose. Often a short page that links to the spec. | `concepts/` |

If you need to explain *why* in the middle of a how-to, write one sentence and link to the explanation. If you need more than one sentence, the explanation page is missing.

## Lead with the answer

- **The first sentence says what the page lets you do**, and when you'd need it. No "In this guide, we will…".
- **The first thing under a heading is the thing**: the command, the rule, the answer. Context comes after, if at all.
- **Warnings go before the step they're about.** A warning after a command arrives once the reader has already run it.
- **Numbers, not adjectives:** "lasts 15 minutes", "1–2 GB per browser", not "a while", "lightweight".

## Voice

The README is the model: short sentences, plain words, concrete facts. Write the way the UI talks (`design/DESIGN.md`, rule 6: functional labels stay plain).

- Second person, present tense, active voice: "BrowserPier stops the browser", not "the browser will be stopped".
- One idea per sentence. Most sentences under 25 words.
- Use the exact name of the thing on screen, in the API, or in the error. Bold UI labels: **Take control**, **Done, hand back**.
- Say what it doesn't do, plainly. "It doesn't fake fingerprints." Readers trust a page that names its limits.
- No marketing on docs pages. The landing page sells; the docs explain.

### Words we don't use

They add length and no meaning, or claim something a reader can't check.

| Don't write | Write instead |
|---|---|
| simply, just, easily, straightforward | (delete it) |
| seamless, powerful, robust, comprehensive, cutting-edge | (delete it, or say what it does) |
| leverage, utilize, facilitate | use, help |
| it's important to note that, note that | (state the fact) |
| in order to | to |
| please | (delete it in instructions) |
| e.g. / i.e. | for example / that is |
| click here, this link | the destination's name: "the firewall settings" |
| the user | you (or "the person" in handoffs, as the UI does) |

## Terms

One word for one thing, everywhere. Agents treat different words as different things, and so do careful readers.

| Term | Means | Not |
|---|---|---|
| **profile** | A saved browser identity with its own cookies and logins, such as `github`. | account, identity, context |
| **browser** | The Chromium running for a profile. Used in prose. | instance, container (unless you mean the Docker container) |
| **session** | A running browser on one profile, as the API sees it (`/api/sessions/{id}`). At most one per profile. | connection |
| **handoff** | An agent asking a person to take over a browser for a step. | escalation, takeover, human-in-the-loop |
| **handoff link** | The one-time URL a handoff gives. | magic link, invite |
| **agent token** | The bearer token an agent uses (`bp_live_…`). | API key |
| **admin password** | The web UI's password. | login, master password |
| **handoff PIN** | The 4–6 digit PIN for handoff links. | passcode |
| **API mode**, **Visual mode** | The two ways to drive a browser. Capitalised as here. | CDP mode, human mode |
| **live view** | The browser's screen streamed to the web UI. | VNC, remote desktop |

Add a term here before using it on a page.

## Examples

Every reference entry and every how-to has at least one example a reader can copy and run.

- **Complete.** Include the auth header, the full URL and the body. An agent often reads one section alone, so don't rely on an example earlier on the page.
- **Real.** Responses are captured from a real run, then trimmed. Never invent a field. Trim with `…` inside a value, never by dropping a field silently.
- **The same placeholders everywhere:**
  - Instance: `https://pier.example.com`
  - Token: `$PIER_TOKEN`, read from the environment, never pasted in
  - Profile: `github` (or `google` for 2FA examples)
  - Session ID: `$SESSION`, set from the start response
- **curl first**, then Python or TypeScript where the reader would use a library (Playwright, the MCP config).
- **Every code block has a language**: `sh`, `json`, `python`, `ts`, `toml`.
- **Show what happens next.** After a command, say what the reader should see, or show the response.

## Templates

### Reference entry

````markdown
## Short name of what it does

`METHOD /path`

One or two sentences: what it does, and the one fact a caller must know.

### Request

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | What it is. Limits. Default if omitted. |

### Response

`201 Created` when …, `200 OK` when ….

```json
{ "…": "…" }
```

| Field | Description |
|---|---|

### Example

```sh
curl …
```

### Errors

| Status | `error` | What to do |
|---|---|---|
| 404 | `no such session` | … |
````

- **Errors quote the server's message exactly**, so a reader searching for an error they got lands on the right page.
- **Every field is listed**, including ones that only appear sometimes. Say when they appear.
- **MCP tools** use the same template, with the tool's arguments in place of the request body.

### How-to

````markdown
---
title: Verb phrase that names the goal
description: One sentence, used by search and by llms.txt.
---

One or two sentences: what you'll have at the end, and when you'd want it.

**Before you start:** what must already be true, as a short list, linked.

## 1. First step

What to do, then the command, then what you should see.

## 2. …

## If something goes wrong

The likely failures, by the message the reader will see.

## Related

- Links to the reference entries used above.
````

Number steps in headings when the order matters, so the step numbers show in the page outline and in links.

### Tutorial

Like a how-to, but it assumes nothing, makes every choice for the reader, and ends with a working result. Link to the how-tos for the other paths.

## Pages that agents read well

- **Plain Markdown.** Pages are `.md`, not `.mdx`, so the Markdown version of each page (`…/page.md`) is the page. Starlight asides (`:::note`, `:::caution`) are fine; components that hide content in tabs or JavaScript are not.
- **Sections that stand alone.** No "as above" or "see the previous section". Repeat the fact, or link to it by name.
- **Headings that say what's under them:** "Hand a browser to a person", not "Handoffs"; "Errors", not "What could go wrong?".
- **A description in every page's front matter.** It feeds search, link previews and `llms.txt`.
- **Versions:** mark what's newer than the oldest supported release with *Since 0.3*, after the heading.

## Accessibility

- **Headings in order**, with no skipped levels. The page title is the only `#`.
- **Link text names the destination.** A screen reader's list of links should make sense on its own.
- **Alt text says what the image shows**, as the README does: "A handoff on a phone: claude needs you on google to enter the 2FA code". No text that exists only in an image; no screenshots of code or terminals.
- **Asides are labelled**, never told apart by colour alone. Use them rarely: `:::caution` for what can lose data or expose a profile, `:::note` for what's easy to miss. If every section has one, none of them work.
- **Plain language.** Guides should read at about grade 8. Define jargon (CDP, TURN, Xvfb) the first time it appears on a page, or link to the glossary.
- **Tables stay narrow** (four columns at most) so they read on a phone. Handoffs happen on phones; their docs get opened there too.
- **Check both themes and a 375 px wide window** before calling a page done.

## Before you call a page done

1. It does one job, and its title says which.
2. Every example runs as written against a real instance, and the responses are real.
3. Every error message quoted matches the code.
4. It has no word from the "don't write" list.
5. A fresh agent, given only this page and a task it covers, can do the task.
6. It reads on a phone, in light and dark.
