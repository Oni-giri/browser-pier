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

Neutrals are Tailwind's cool **slate** scale, not neutral grey, so they sit naturally on the blue page.

| Token | Value | Use |
|---|---|---|
| `page` | `#EAF1FF` + background recipe below | App page background |
| `surface` | `#FFFFFF` | Cards, modals, inputs |
| `ink` | `#0F172A` (slate-900) | Text, primary buttons, dark code cards |
| `ink-body` | `#334155` (slate-700) | Body text inside cells |
| `ink-2` | `#475569` (slate-600) | Secondary text |
| `ink-3` | `#64748B` (slate-500) | Muted text, captions |
| `line` | `#E2E8F0` (slate-200) | Input borders, button rings |
| `line-soft` | `#EEF2F7` | Dividers inside cards |
| `well` | `#F1F5F9` (slate-100) | Recessed panels inside cards: previews, code backgrounds, segmented controls |
| `blue` | `#2F6BFF` (text `#1D4ED8`, tint `#EEF3FF`) | "Take control", focus, selection, links |
| `orange` | `#FF6A3D` (text `#C2410C`, tint `#FFF1EB`) | "Needs you" |
| `green` | `#16A34A` (text `#15803D`, tint `#ECFDF3`) | Running, success |
| `red` | `#DC2626` (text `#B91C1C`, tint `#FEF2F2`) | Failed, destructive |
| `grey dot` | `#94A3B8` (slate-400) | Stopped |

No warm or earthy backgrounds. Shadows are tinted blue (`rgba(37,64,140,…)`), never pure black.

**Surfaces (four levels, used the same way on every screen)**

1. **Page:** pale sky blue. Layers, top to bottom: a blue light from the top left, a faint cyan light from the top right, a fine grain, then a 48px hairline grid in blue that fades out by about 560px so dense content sits on a clean surface. One CSS `background` rule; copy it from `screens/overview.html` (the root `div`).
2. **Card:** white, radius 16, shadow `0 0 0 1px rgba(37,64,140,0.07), 0 1px 2px rgba(37,64,140,0.06), 0 10px 28px -12px rgba(37,64,140,0.20)`.
3. **Well:** `#F1F5F9` panels inside cards for content that sits "below" the card (browser previews, lists, code).
4. **Emphasis:** only for what needs a human. Orange-tinted top `linear-gradient(180deg, #FFF4EE 0, #FFFFFF 140px)` with ring `0 0 0 1px rgba(255,106,61,0.30)` and a warm shadow. At most one per screen.

The header is frosted: `rgba(255,255,255,0.72)` with `backdrop-filter: blur(14px) saturate(160%)`, sticky, with a 1px blue-tinted bottom shadow.

**Type:** Geist for everything; Geist Mono only for things you copy (see rules). Page titles 26px/600, letter-spacing −0.8px. Card titles 14px/500 in `ink-2`, or 16px/600 for prominent cells. Body 14px, captions 12–13px. Numbers that change use `font-variant-numeric: tabular-nums`.

**Shape**
- Cards ("cells"): see Surfaces above; padding 18–20.
- Buttons: radius 8, min height 32–36. Primary is `ink` with `inset 0 1px 0 rgba(255,255,255,0.12)`; secondary is white with the `line` ring.
- Pills and chips: radius 999, 12px/500, tint background plus a 6px dot.
- Dialogs: centred modal, radius 16, scrim `rgba(15,23,42,0.32–0.4)`. No side sheets.

**Layout:** content max width 1320px with 28px side padding. Pages use a 12-column bento grid with a 14px gap; cells span with `grid-column: span N` and go full width under 960px.

## Rules

1. **One header on every app page:** logo (→ Overview), nav (Overview · Profiles · Agents · Settings), the orange "N needs you" chip (→ the waiting profile's live view; hidden at zero), search (⌘K), and an avatar linking to account and sign-out.
2. **One colour per action:**
   - "Take control" is always blue, including on the phone handoff page.
   - "Hand back" is neutral: white with a ring in the app, `ink` as the phone page's main button.
   - Start and Retry are `ink`; Delete is red.
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
- Toast (bottom centre, `ink`) and unsaved-changes bar (Settings)
- Code block (dark, with tabs and a Copy button)
