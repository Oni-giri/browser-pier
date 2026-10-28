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
      navigator.clipboard.writeText(copy.dataset.copy).then(
        () => toast(copy.dataset.copied || "Copied"),
        () => toast("Couldn't copy. Select the text and copy it yourself.", true));
      closeMenus();
    }
    // Clicking outside a ⋯ menu closes it.
    $$("details.menu[open]").forEach((d) => { if (!d.contains(e.target)) d.open = false; });
  });
  function closeMenus() { $$("details.menu[open]").forEach((d) => { d.open = false; }); }
  // A dialog htmx put in #modal goes away when closed.
  document.addEventListener("close", (e) => { if (e.target.parentElement && e.target.parentElement.id === "modal") e.target.remove(); }, true);
  function openMarked(root) { $$("dialog[data-open]", root).forEach(open); }

  // Forms say they're busy, so a slow start isn't clicked twice.
  document.addEventListener("submit", (e) => {
    const form = e.target;
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
    openMarked(e.target);
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
      if (each) $("#ram-fits").textContent = Math.floor(limit / each);
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

  openMarked(document);
})();
