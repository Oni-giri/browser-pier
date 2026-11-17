// BrowserPier web UI: the small behaviours htmx doesn't cover. No build step.
"use strict";
(() => {
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

  // Toasts: bottom centre, gone after a few seconds.
  function toast(text, error) {
    const box = $("#toasts");
    const t = document.createElement("div");
    t.className = "toast" + (error ? " error" : "");
    t.textContent = (error ? "" : "✓ ") + text;
    box.append(t);
    setTimeout(() => t.remove(), error ? 6000 : 2500);
  }
  $$("#toasts .toast").forEach((t) => setTimeout(() => t.remove(), t.classList.contains("error") ? 8000 : 3000));

  // Dialogs. A button with data-dialog opens one, filling {name}-style
  // placeholders from its own data- attributes, as text.
  function fill(dialog, data) {
    const sub = (s) => s.replace(/\{([a-z-]+)\}/g, (_, k) => (k in data ? data[k] : ""));
    $$("[data-text]", dialog).forEach((el) => { el.textContent = sub(el.dataset.text); });
    $$("form[data-action]", dialog).forEach((f) => {
      const enc = {};
      for (const k in data) enc[k] = encodeURIComponent(data[k]);
      f.action = f.dataset.action.replace(/\{([a-z-]+)\}/g, (_, k) => enc[k] || "");
    });
  }
  function open(dialog) {
    if (!dialog || dialog.open) return;
    dialog.showModal();
    const first = $("[autofocus]", dialog) || $("input:not([type=hidden]):not([readonly]), select, textarea", dialog);
    if (first) first.focus();
  }
  document.addEventListener("click", (e) => {
    const opener = e.target.closest("[data-dialog]");
    if (opener) {
      const dialog = document.getElementById(opener.dataset.dialog);
      const data = { ...opener.dataset };
      data["agent-warning"] = data.agent ? `${data.agent} is using this browser right now and will lose its session. ` : "";
      closeMenus();
      fill(dialog, data);
      open(dialog);
      return;
    }
    if (e.target.closest("[data-close]")) { e.target.closest("dialog").close(); return; }
    // A click on the backdrop closes a dialog.
    if (e.target.tagName === "DIALOG" && e.target.open) {
      const r = e.target.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) e.target.close();
    }
    const copy = e.target.closest("[data-copy]");
    if (copy) {
      copyText(copy.dataset.copy, copy).then(
        () => toast(copy.dataset.copied || "Copied"),
        () => toast("Couldn't copy. Select the text and copy it yourself.", true));
      closeMenus();
    }
    // Clicking outside a ⋯ menu closes it.
    $$("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.open = false; });
  });
  // copyText puts text on the clipboard. navigator.clipboard exists only in
  // a secure context (HTTPS or localhost); over plain HTTP, as on Umbrel, a
  // selected off-screen textarea and execCommand("copy") do it instead,
  // while the click still counts as the user's. It never throws: a failed
  // copy is a rejected promise.
  function copyText(text, from) {
    try {
      const api = window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText;
      if (api) return navigator.clipboard.writeText(text).catch(() => copyBySelection(text, from));
      copyBySelection(text, from); // now, while the click still counts
      return Promise.resolve();
    } catch (err) {
      return Promise.reject(err);
    }
  }
  // copyBySelection copies through a temporary textarea, and puts focus,
  // selection and scroll back as they were. Inside an open modal dialog the
  // textarea goes in the dialog, as the rest of the page is inert.
  function copyBySelection(text, from) {
    const host = (from && from.closest("dialog[open]")) || document.body;
    const active = document.activeElement;
    const sel = document.getSelection();
    const ranges = [];
    for (let i = 0; sel && i < sel.rangeCount; i++) ranges.push(sel.getRangeAt(i));
    const x = window.scrollX, y = window.scrollY;
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.readOnly = true;
    ta.tabIndex = -1;
    ta.setAttribute("aria-hidden", "true");
    Object.assign(ta.style, { position: "fixed", top: "0", left: "-9999px", width: "1px", height: "1px", opacity: "0", pointerEvents: "none" });
    host.append(ta);
    let ok = false;
    try {
      ta.focus({ preventScroll: true });
      ta.select();
      ta.setSelectionRange(0, text.length);
      ok = document.execCommand("copy");
    } finally {
      ta.remove();
      if (active && active !== document.body && active.focus) active.focus({ preventScroll: true });
      if (sel) { sel.removeAllRanges(); ranges.forEach((r) => sel.addRange(r)); }
      if (window.scrollX !== x || window.scrollY !== y) window.scrollTo(x, y);
    }
    if (!ok) throw new Error("copy refused");
  }
  // A read-only value you can copy is selected whole when it gets the
  // focus. Focused by a press, it is selected once the click is done, and
  // only if the press didn't drag over part of it; a click on it when it
  // has the focus already puts the caret down, so a drag can select part.
  const copyField = (el) => el.matches && el.matches(".copy-row input[readonly]");
  let pressed = null;
  document.addEventListener("pointerdown", (e) => {
    pressed = copyField(e.target) && document.activeElement !== e.target ? e.target : null;
  }, true);
  document.addEventListener("focusin", (e) => { if (copyField(e.target) && e.target !== pressed) e.target.select(); });
  document.addEventListener("click", (e) => {
    const f = pressed;
    pressed = null;
    if (f && f === e.target && f.selectionStart === f.selectionEnd) f.select();
  });
  function closeMenus() { $$("details.menu[open]").forEach((d) => { d.open = false; }); }
  // A dialog htmx put in #modal goes away when closed.
  document.addEventListener("close", (e) => { if (e.target.parentElement && e.target.parentElement.id === "modal") e.target.remove(); }, true);
  // A dialog marked data-open opens once, when it arrives.
  function openMarked() { $$("dialog[data-open]").forEach((d) => { d.removeAttribute("data-open"); open(d); }); }

  // Forms say they're busy, so a slow start isn't clicked twice.
  document.addEventListener("submit", (e) => {
    const form = e.target;
    // A download doesn't leave the page: close the dialog and say so.
    if (form.dataset.download) {
      const dialog = form.closest("dialog");
      setTimeout(() => { form.reset(); if (dialog) dialog.close(); }, 0);
      toast(form.dataset.download);
      return;
    }
    const label = form.dataset.busy;
    if (!label) return;
    const btns = $$("button[type=submit]", form).concat(form.id ? $$(`button[type=submit][form="${form.id}"]`) : []);
    const clicked = e.submitter && btns.includes(e.submitter) ? e.submitter : btns[btns.length - 1];
    setTimeout(() => btns.forEach((b) => { b.disabled = true; }), 0); // after the browser has read the submitter's value
    if (clicked) { clicked.dataset.label = clicked.textContent; clicked.textContent = label; }
    form.dataset.submitted = "1";
  });
  document.addEventListener("htmx:afterRequest", (e) => {
    const form = e.target.closest && e.target.closest("form");
    if (!form) return;
    $$("button[type=submit]", form).forEach((b) => { b.disabled = false; if (b.dataset.label) b.textContent = b.dataset.label; });
  });
  document.addEventListener("htmx:responseError", (e) => {
    const xhr = e.detail.xhr;
    toast(xhr.status === 0 ? "Can't reach BrowserPier." : (xhr.responseText || "Something went wrong.").trim().slice(0, 200), true);
  });
  $$("form[data-autosubmit]").forEach((f) => f.requestSubmit());

  // Polling waits while a menu or dialog is open, or the tab is hidden, so
  // a refresh never closes what you're doing.
  document.addEventListener("htmx:beforeRequest", (e) => {
    if (!e.target.hasAttribute || !e.target.hasAttribute("data-poll")) return;
    if (document.hidden || $("details.menu[open]") || $("dialog[open]") || e.target.contains(document.activeElement) && document.activeElement.matches("input, textarea, select")) e.preventDefault();
  });
  document.addEventListener("htmx:afterSwap", (e) => {
    openMarked();
    // The page light follows the header's "needs you" chip.
    if (e.target.id === "chip") document.body.classList.toggle("has-help", !!$(".chip-help", e.target));
    // The live view reloads when its browser starts or stops.
    const page = $("[data-profile][data-state]");
    const side = $("[data-side-state]", e.target);
    if (page && side) {
      const live = (s) => ["running", "needs_help"].includes(s);
      if (live(page.dataset.state) !== live(side.dataset.sideState) || (page.dataset.state === "starting") !== (side.dataset.sideState === "starting")) location.reload();
    }
  });

  // Shortcuts: ⌘K or Ctrl+K searches, N makes something new.
  document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
      const s = $("#search");
      if (s && s.offsetParent) { e.preventDefault(); s.focus(); s.select(); }
      return;
    }
    if (e.key === "Escape") closeMenus();
    if (e.metaKey || e.ctrlKey || e.altKey || e.target.matches("input, textarea, select, [contenteditable]") || $("dialog[open]")) return;
    if (e.key === "n" || e.key === "N") {
      const btn = $("[data-key=n]");
      if (btn) { e.preventDefault(); btn.click(); }
    }
  });

  // Ticking a choice can select the option it belongs to.
  // The icon picker's preview: Auto falls back to the profile's own default.
  document.addEventListener("change", (e) => {
    if (!e.target.matches("[name=icon_glyph], [name=icon_color]")) return;
    const box = $("#icon-preview", e.target.form);
    if (!box) return;
    const pick = (n, d) => ($(`[name=${n}]:checked`, e.target.form) || {}).value || box.dataset[d];
    box.className = "icon c-" + pick("icon_color", "color");
    $("use", box).setAttribute("href", "#g-" + pick("icon_glyph", "glyph"));
  });
  document.addEventListener("change", (e) => {
    const sel = e.target.dataset && e.target.dataset.select;
    if (sel && e.target.checked) { const el = $(sel); if (el) el.checked = true; }
  });

  // Appearance: the page switches at once, as the choice is saved, and back
  // if saving fails. System drops the attribute, and the page follows the
  // device again.
  const bars = { light: "#EAF1FF", dark: "#070C18" };
  // Choices made quickly queue up, so each one is numbered and each answer
  // is about the choice it carried. The number goes up before htmx reads
  // the form, hence the capture phase.
  let savedTheme = ($("form[data-appearance] input:checked") || {}).value;
  let choice = 0;
  document.addEventListener("change", (e) => {
    if (!e.target.closest("form[data-appearance]")) return;
    choice++;
    applyTheme(e.target.value);
  }, true);
  document.addEventListener("htmx:configRequest", (e) => {
    if (e.target.matches && e.target.matches("form[data-appearance]")) e.detail.parameters.choice = choice;
  });
  document.addEventListener("htmx:afterRequest", (e) => {
    const form = e.target.matches && e.target.matches("form[data-appearance]") && e.target;
    if (!form) return;
    const sent = e.detail.requestConfig.parameters;
    const latest = +sent.choice === choice;
    if (e.detail.successful) {
      savedTheme = sent.theme;
      if (latest) toast("Appearance saved");
      return;
    }
    if (!latest) return; // a newer choice is on its way
    const was = $(`input[value="${savedTheme}"]`, form);
    if (was) was.checked = true;
    applyTheme(savedTheme);
    if (!e.detail.xhr || !e.detail.xhr.status) toast("Can't reach BrowserPier.", true);
  });
  function applyTheme(t) {
    const root = document.documentElement;
    if (t === "light" || t === "dark") root.dataset.theme = t; else delete root.dataset.theme;
    $("meta[name=color-scheme]").content = t === "system" ? "light dark" : t;
    $$("meta[name=theme-color]").forEach((m) => m.remove());
    for (const [k, c] of Object.entries(bars)) {
      if (t !== "system" && t !== k) continue;
      const m = document.createElement("meta");
      m.name = "theme-color"; m.content = c;
      if (t === "system") m.media = `(prefers-color-scheme: ${k})`;
      document.head.append(m);
    }
  }

  // Test proxy: the field's value goes to the server, which tests it from
  // a short-lived container and answers with the result to show under the
  // field. Nothing is saved, and the button doesn't submit the form. An
  // empty field has nothing to test; an edit clears a result, so a stale
  // one never looks current.
  const proxyBox = (el) => el.closest && el.closest("[data-proxy]");
  document.addEventListener("input", (e) => {
    const box = e.target.name === "proxy" && proxyBox(e.target);
    if (!box) return;
    const out = $("[data-proxy-result]", box);
    out.hidden = true;
    out.textContent = "";
    const btn = $("[data-proxy-test]", box);
    if (!btn.dataset.label) btn.disabled = !e.target.value.trim();
  });
  document.addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-proxy-test]");
    if (!btn || btn.disabled) return;
    const box = proxyBox(btn);
    const input = $("input[name=proxy]", box);
    const out = $("[data-proxy-result]", box);
    const sent = input.value;
    if (!sent.trim()) return;
    const body = new URLSearchParams({ proxy: sent, form: btn.dataset.proxyTest, profile: btn.dataset.profile || "" });
    btn.dataset.label = btn.textContent;
    btn.textContent = "Testing…";
    btn.disabled = true;
    out.hidden = false;
    out.innerHTML = '<span class="hint" style="margin:0">Testing the proxy from this server. This takes a few seconds, longer if the browser image is still to download.</span>';
    let html = "";
    try {
      const r = await fetch("/proxy/test", { method: "POST", body, credentials: "same-origin" });
      if (r.status === 401) html = '<p class="errors" style="margin:0">You were signed out. Reload the page and sign in again.</p>';
      else if (r.headers.get("X-BrowserPier-Fragment") === "proxy-result") html = await r.text();
      if (!html) throw new Error("no answer");
    } catch (err) {
      html = '<p class="errors" style="margin:0">Can\'t reach BrowserPier.</p>';
    }
    btn.textContent = btn.dataset.label;
    delete btn.dataset.label;
    btn.disabled = !input.value.trim();
    // An answer about a value since edited would look current: drop it.
    if (input.value !== sent) return;
    out.innerHTML = html; // BrowserPier's own fragment, every value escaped
    out.hidden = false;
  });

  // Code tabs.
  $$("[data-tabs]").forEach((box) => {
    box.addEventListener("click", (e) => {
      const tab = e.target.closest("[data-tab]");
      if (!tab) return;
      $$("[data-tab]", box).forEach((t) => t.setAttribute("aria-selected", t === tab ? "true" : "false"));
      $$("[data-panel]", box).forEach((p) => { p.hidden = p.dataset.panel !== tab.dataset.tab; });
    });
  });

  // Settings: the memory slider explains itself, and changes wait in the
  // unsaved-changes bar until saved or discarded.
  const ram = $("#ram");
  function showRAM() {
    if (!ram) return;
    const pct = +ram.value, host = +ram.dataset.host, each = +ram.dataset.each;
    $("#ram-out").textContent = pct + "%";
    $("#ram-mark").style.left = pct + "%";
    if (host) {
      const limit = host * pct / 100;
      $("#ram-gb").textContent = limit >= 2 ** 30 ? (limit / 2 ** 30).toFixed(1) + " GB" : Math.floor(limit / 2 ** 20) + " MB";
      // Other apps share the host: count only what they leave.
      const room = +ram.dataset.room || Infinity;
      if (each) {
        const fits = Math.floor(Math.min(limit, room) / each);
        $("#ram-fits").textContent = fits;
        $("#ram-fits-word").textContent = fits === 1 ? "browser" : "browsers";
      }
    }
  }
  const tracked = $("form[data-track]");
  if (tracked) {
    const fields = () => $$(`[form="${tracked.id}"]`).filter((el) => el.name);
    const value = (el) => (el.type === "radio" || el.type === "checkbox" ? el.checked : el.value);
    const initial = new Map(fields().map((el) => [el, value(el)]));
    const bar = $("#unsaved");
    const check = () => { bar.hidden = !fields().some((el) => value(el) !== initial.get(el)); };
    document.addEventListener("input", (e) => { if (e.target === ram) showRAM(); if (initial.has(e.target)) check(); });
    document.addEventListener("change", (e) => { if (initial.has(e.target)) check(); });
    $("[data-discard]").addEventListener("click", () => {
      initial.forEach((v, el) => { if (el.type === "radio" || el.type === "checkbox") el.checked = v; else el.value = v; });
      showRAM(); check();
    });
    addEventListener("beforeunload", (e) => { if (!bar.hidden && !tracked.dataset.submitted) e.preventDefault(); });
  }

  openMarked();
})();
