# BrowserPier — Design

The reference for the web UI (PROJ-11), the handoff page (PROJ-10) and the landing page. The screens in `screens/` are the source of truth for layout, copy and states; this file is the rules that hold across them. If a screen and this file disagree, raise it.

## Files

| Screen | File | Route (proposed) | Screenshot |
|---|---|---|---|
| Login | `screens/login.html` | `/login` | `screenshots/login.png` |
| Overview | `screens/overview.html` | `/` | `screenshots/overview.png` |
| Profiles (grid + table) | `screens/profiles.html` | `/profiles` | `screenshots/profiles.png` |
| New profile (modal over Profiles) | `screens/new-profile.html` | `/profiles/new` (modal, htmx) | `screenshots/new-profile.png` |
| Live view | `screens/live-view.html` | `/profiles/{name}` | `screenshots/live-view.png` |
| Agents (tokens, quick start) | `screens/agents.html` | `/agents` | `screenshots/agents.png` |
| Settings | `screens/settings.html` | `/settings` | `screenshots/settings.png` |
| Handoff link (phone) | `screens/handoff.html` | `/h/{token}` | `screenshots/handoff.png` |
| Landing page (marketing site) | `screens/landing.html` | separate site | `screenshots/landing.png` |

### Viewing the screens

The screens were made on a design canvas. `screens/support.js` is a small stand-in runtime so they open in a normal browser and are clickable:

```sh
cd design/screens && python3 -m http.server
# open http://localhost:8000/overview.html
```

Each file is HTML with inline styles, `{{value}}` placeholders, `<sc-if>` / `<sc-for>` blocks and a small `Component` class at the bottom holding sample data and state (which state each button switches to, timers, dialogs). Read that class to see every state a screen has. Some screens take props, for example `profiles.html?props={"empty":true}` for the empty state or `handoff.html?props={"start":"done"}` for the other handoff steps.

When building the real UI, translate these into Go templates + htmx with a shared stylesheet. Don't copy the inline styles or the stand-in runtime.

## Tokens

**Colour**

| Token | Value | Use |
|---|---|---|
| `bg` | `#FAFAFA` | Page background |
| `surface` | `#FFFFFF` | Cards, header, inputs |
| `ink` | `#09090B` | Text, primary buttons |
| `ink-2` | `#52525B` | Secondary text |
| `ink-3` | `#71717A` | Muted text, captions |
| `ink-body` | `#3F3F46` | Body text inside cells |
| `line` | `#E4E4E7` | Card rings, input borders |
| `line-soft` | `#F0F0F2` | Dividers inside cards |
| `fill` | `#F4F4F5` | Segmented controls, chips, code backgrounds |
| `blue` | `#2F6BFF` (text `#1D4ED8`, tint `#EEF3FF`) | "Take control", focus, selection, links |
| `orange` | `#FF6A3D` (text `#C2410C`, tint `#FFF1EB`) | "Needs you" |
| `green` | `#16A34A` (text `#15803D`, tint `#ECFDF3`) | Running, success |
| `red` | `#DC2626` (text `#B91C1C`, tint `#FEF2F2`) | Failed, destructive |
| `grey dot` | `#A1A1AA` | Stopped |

Cool neutrals only. No warm or earthy backgrounds.

**Type:** Geist for everything; Geist Mono only for things you copy (see rules). Page titles 26px/600, letter-spacing −0.8px. Card titles 14px/500 in `ink-2`, or 16px/600 for prominent cells. Body 14px, captions 12–13px. Numbers that change use `font-variant-numeric: tabular-nums`.

**Shape**
- Cards ("cells"): white, radius 16, ring `0 0 0 1px #E4E4E7, 0 1px 2px rgba(9,9,11,0.04)`, padding 18–20.
- Buttons: radius 8, min height 32–36. Primary is black with `inset 0 1px 0 rgba(255,255,255,0.12)`; secondary is white with the `line` ring.
- Pills and chips: radius 999, 12px/500, tint background plus a 6px dot.
- Dialogs: centred modal, radius 16, scrim `rgba(9,9,11,0.32–0.4)`. No side sheets.

**Layout:** content max width 1320px with 28px side padding. Pages use a 12-column bento grid with a 14px gap; cells span with `grid-column: span N` and go full width under 960px.

## Rules

1. **One header on every app page:** logo (→ Overview), nav (Overview · Profiles · Agents · Settings), the orange "N needs you" chip (→ the waiting profile's live view; hidden at zero), search (⌘K), and an avatar linking to account and sign-out.
2. **One colour per action:**
   - "Take control" is always blue, including on the phone handoff page.
   - "Hand back" is neutral: white with a ring in the app, black as the phone page's main button.
   - Start and Retry are black; Delete is red.
3. **Mono only for things you copy:** URLs, tokens, CDP endpoints, profile slugs in inputs, code. Names, scopes, times and sizes stay in Geist.
4. **Overview summarises; it doesn't duplicate.** Each cell links to the page that owns the controls. The one exception is "Take control" on the waiting profile.
5. **Status vocabulary:** Needs you (orange), Running (green), Starting (blue, spinner), Stopped (grey), Failed (red, with the exit reason). The same words appear everywhere.
6. **Functional labels stay plain.** No nautical wordplay on buttons or statuses; the theme is for the logo and marketing only.
7. **Show real numbers or none.** No invented comparisons ("31% faster"). Placeholder figures in the screens are sample data, not copy.

## Components to build once

- Header (with the needs-you chip)
- Cell (card) with title row and optional "→" link
- Buttons: primary, secondary, blue (take control), destructive
- Status pill
- Segmented control (filters, mode, policy)
- Profile card with live preview, plus the table row variant
- Centred modal and alert dialog
- Toast (bottom centre, black) and unsaved-changes bar (Settings)
- Code block (dark, with tabs and a Copy button)
