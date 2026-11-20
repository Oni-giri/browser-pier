// BrowserPier live view: watch a browser over WebRTC, and take control to
// send mouse and keyboard over a data channel. No build step.
"use strict";
(() => {
  const root = document.querySelector("[data-profile]");
  const profile = root.dataset.profile;
  // A handoff page points these at its own endpoints; its offer answers
  // with control from the start.
  const offerURL = root.dataset.offer || `/live/${encodeURIComponent(profile)}/offer`;
  const statusURL = root.dataset.status || `/live/${encodeURIComponent(profile)}/status`;
  const iceURL = root.dataset.ice || `/live/${encodeURIComponent(profile)}/ice`;
  // A page whose offer brings control (a handoff) keeps its own pill until
  // the offer has answered: until then the status says "Needs help", as
  // nobody has control yet.
  let offerAnswered = !root.dataset.offer;
  // connectError, once set, is why this view couldn't connect: it stays in
  // the pill and the hint, which say to reload.
  let connectError = "";
  // ?ice=relay connects only through the TURN server: a way to check it works.
  const relayOnly = new URLSearchParams(location.search).get("ice") === "relay";
  const video = document.getElementById("screen");
  const stage = document.getElementById("stage");
  const who = document.getElementById("who");
  // Take control / Hand back: the page's button, and the bar's for full
  // screen.
  const takeBtns = [...document.querySelectorAll("[data-take]")];
  const viewer = document.querySelector("[data-viewer]");
  const fullBtn = document.getElementById("full");
  const typeBtn = document.getElementById("type");
  const fitBtn = document.getElementById("fit");
  const hint = document.getElementById("hint");
  const note = document.getElementById("note");
  const touch = matchMedia("(pointer: coarse)").matches;
  // On a Mac, ⌘ is sent as Ctrl: the browser runs on Linux, where Super
  // does nothing and Ctrl makes the shortcuts (PROJ-194).
  const mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || "") || (navigator.userAgentData && navigator.userAgentData.platform === "macOS");
  const cmd = mac ? "⌘" : "Ctrl+";
  // maxPaste is the most a paste types: the container's cap at instant pace.
  const maxPaste = 2000;

  let dc = null;
  let control = false;
  let keyboard = false; // keys go to the browser (released with Esc)
  let full = false; // the viewer fills the screen
  let status = {};
  let zoom = 1, panX = 0, panY = 0;

  function say(text) {
    note.textContent = text;
    note.classList.add("show");
    clearTimeout(say.t);
    say.t = setTimeout(() => note.classList.remove("show"), 2500);
  }
  function send(msg) { if (dc && dc.readyState === "open") dc.send(JSON.stringify(msg)); }

  // Mouse moves are coalesced to one per animation frame.
  let queue = [], pendingMove = null, scheduled = false;
  function flush() {
    scheduled = false;
    if (pendingMove) { queue.push(pendingMove); pendingMove = null; }
    if (queue.length) { send({ type: "input", events: queue }); queue = []; }
  }
  function push(ev) {
    if (!control) return;
    if (ev.t === "move") pendingMove = ev; else { if (pendingMove) { queue.push(pendingMove); pendingMove = null; } queue.push(ev); }
    if (!scheduled) { scheduled = true; requestAnimationFrame(flush); }
  }

  // Screen coordinates from a point on the page, through object-fit and zoom.
  function toScreen(clientX, clientY) {
    const r = video.getBoundingClientRect();
    const vw = video.videoWidth, vh = video.videoHeight;
    if (!vw || !vh) return null;
    const scale = Math.min(r.width / vw, r.height / vh);
    const ox = r.left + (r.width - vw * scale) / 2, oy = r.top + (r.height - vh * scale) / 2;
    const x = Math.floor((clientX - ox) / scale), y = Math.floor((clientY - oy) / scale);
    if (x < 0 || y < 0 || x >= vw || y >= vh) return null;
    return { x, y };
  }

  function render() {
    const s = status.state;
    let label = "Stopped", cls = "";
    if (s === "running" || s === "needs_help") {
      if (control) { label = "You have control"; cls = "you"; }
      else if (s === "needs_help") { label = "Needs help"; cls = "help"; }
      else if (status.in_use_by && status.in_use_by.length) { label = "Agent driving: " + status.in_use_by.join(", "); cls = "agent"; }
      else if (status.human_control) { label = "Someone else has control"; }
      else { label = "Idle"; }
    } else if (s === "starting") { label = "Starting…"; }
    if (s === undefined && control) { label = "You have control"; cls = "you"; }
    // Until the first status, the page's own words stay ("Connecting…",
    // or the handoff page's "You're driving", in blue, until its offer
    // has answered).
    if (connectError) { label = connectError; cls = ""; }
    if (((s !== undefined || control) && offerAnswered) || connectError) {
      who.textContent = label;
      who.className = "pill " + cls;
    }
    // While it waits for help the agent is paused, so a person can always take over.
    const agentDriving = s === "running" && !!(status.in_use_by && status.in_use_by.length);
    for (const b of takeBtns) {
      b.textContent = control ? "Hand back" : "Take control";
      b.classList.toggle("blue", !control);
      b.disabled = !dc || dc.readyState !== "open" || (!control && (agentDriving || status.human_control || (s !== "running" && s !== "needs_help")));
    }
    typeBtn.hidden = !(control && touch);
    fitBtn.hidden = !touch;
    stage.classList.toggle("control", control);
    hint.textContent = (control
      ? (touch ? `Tap to click. Pinch to zoom. ${typeBtn.textContent} sends text.` : keyboard ? `Your keyboard goes to the browser, ${cmd}C and ${cmd}V included. Esc releases it.` : "Click the screen to send your keyboard to the browser.")
      : "View only. Take control to use the mouse and keyboard.") + (full && !touch ? " Esc leaves full screen." : "");
    if (connectError) hint.textContent = "Reload the page to try again.";
    if (fullBtn) fullBtn.textContent = full ? "Exit full screen" : "Full screen";
  }

  async function poll() {
    try {
      const r = await fetch(statusURL, { cache: "no-store" });
      if (r.redirected || r.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname); return; }
      status = await r.json();
    } catch (_) { /* keep the last status */ }
    if (control && !status.human_control) { control = false; keyboard = false; }
    render();
  }

  // The TURN server, if BrowserPier has one, with credentials for this
  // viewer. Without them the view still connects where UDP gets through.
  async function iceServers() {
    try {
      const r = await fetch(iceURL, { cache: "no-store" });
      if (r.ok) return (await r.json()).iceServers || [];
    } catch (_) { /* connect without */ }
    return [];
  }

  async function connect() {
    const pc = new RTCPeerConnection({ iceServers: await iceServers(), iceTransportPolicy: relayOnly ? "relay" : "all" });
    pc.addTransceiver("video", { direction: "recvonly" });
    dc = pc.createDataChannel("input", { ordered: true });
    dc.onopen = render;
    dc.onclose = () => { control = false; render(); };
    dc.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.type === "control") {
        control = m.on; keyboard = control && !touch;
        status.human_control = m.on; // until the next poll says otherwise
        if (keyboard) video.focus();
        render(); poll();
      }
      if (m.type === "clipboard") { copied(m.text); }
      if (m.type === "error") { if (copyWait) copyWait.fail(new Error(m.msg)); else say(m.msg); poll(); }
    };
    pc.ontrack = (e) => { video.srcObject = e.streams[0] || new MediaStream([e.track]); };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "closed") { who.textContent = "Disconnected"; who.className = "pill"; setTimeout(() => location.reload(), 3000); }
    };
    await pc.setLocalDescription(await pc.createOffer());
    await new Promise((done) => {
      if (pc.iceGatheringState === "complete") return done();
      pc.onicegatheringstatechange = () => pc.iceGatheringState === "complete" && done();
      // Relayed candidates come last. Once one has, a TURN server the
      // network blocks would only hold the offer up to the cap: BrowserPier
      // learns any candidate that comes later from its connectivity checks.
      // Not every browser has candidate.type: the SDP line says it too.
      pc.onicecandidate = (e) => { if (e.candidate && (e.candidate.type === "relay" || / typ relay( |$)/.test(e.candidate.candidate))) done(); };
      setTimeout(done, 3000);
    });
    const r = await fetch(offerURL, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ sdp: pc.localDescription.sdp }),
    });
    if (!r.ok) { failed((await r.text()).trim() || "Couldn't connect"); return; }
    const answer = await r.json();
    await pc.setRemoteDescription({ type: "answer", sdp: answer.sdp });
    offerAnswered = true;
    if (answer.control) { control = true; keyboard = !touch; status.human_control = true; }
    render();
  }

  for (const b of takeBtns) b.onclick = () => send({ type: control ? "release" : "take" });

  // Full screen (PROJ-193): the Fullscreen API where there is one, else
  // (iPhone Safari, or a refusal) the viewer fixed over the page. Only the
  // picture is scaled: the browser's own window keeps its size.
  function setFull(on) {
    // However full screen ends (Esc, the browser's own exit, the bar's
    // button), the keyboard comes back to this page.
    if (!on && keyboard) { lift(); keyboard = false; }
    full = on;
    viewer.classList.toggle("full", on);
    document.body.classList.toggle("full-open", on);
    render();
  }
  async function leaveFull() {
    if (document.fullscreenElement) { try { await document.exitFullscreen(); } catch (_) { /* gone already */ } }
    setFull(false);
  }
  if (fullBtn && viewer) fullBtn.onclick = async () => {
    if (full) return leaveFull();
    setFull(true);
    if (viewer.requestFullscreen) {
      try { await viewer.requestFullscreen({ navigationUI: "hide" }); } catch (_) { /* the fixed view stays */ }
    }
    if (control && !touch) { keyboard = true; video.focus(); render(); }
  };
  // Esc leaves the Fullscreen API's full screen before a page sees it: it
  // also releases the keyboard, as Esc does outside it.
  document.addEventListener("fullscreenchange", () => {
    if (!document.fullscreenElement && full) setFull(false);
  });
  typeBtn.onclick = () => { const text = prompt("Type or paste into the browser"); if (text) pasteText(text); };

  // Paste (PROJ-194): the person's clipboard text is typed into the
  // browser, at once. It is the paste event's own text, so no permission
  // is asked; Safari, which may not fire one on the stream, gets a
  // readText() after a moment (it shows its own Paste button).
  let pasteWait = 0;
  function pasteText(text) {
    text = String(text || "").replace(/\r\n?/g, "\n");
    if (!text) { say("Only text can be pasted."); return; }
    const n = [...text].length;
    if (n > maxPaste) { say(`That's ${n.toLocaleString()} characters: at most ${maxPaste.toLocaleString()} can be pasted at once.`); return; }
    flush(); // keys pressed before the paste go first
    send({ type: "text", text });
  }
  addEventListener("paste", (e) => {
    if (!control || !keyboard) return;
    clearTimeout(pasteWait); pasteWait = 0;
    e.preventDefault();
    pasteText(e.clipboardData && e.clipboardData.getData("text/plain"));
  });
  function pasteSoon() {
    clearTimeout(pasteWait);
    pasteWait = setTimeout(() => {
      pasteWait = 0;
      if (!navigator.clipboard || !navigator.clipboard.readText) { say("This browser didn't give BrowserPier the text to paste."); return; }
      navigator.clipboard.readText().then(pasteText, () => say("Pasting was refused by this browser."));
    }, 150);
  }

  // Copy (PROJ-194): after the browser's own copy, its clipboard is read
  // once and written to the person's. Never polled: a page can't put text
  // in the person's clipboard without them copying. The write starts in
  // the key's handler, with the text to come, so Safari allows it.
  let copyWait = null;
  function copyFromBrowser() {
    if (copyWait) copyWait.fail(new Error("copied again"));
    let ok, fail;
    const text = new Promise((res, rej) => { ok = res; fail = rej; });
    const timer = setTimeout(() => fail(new Error("the browser didn't answer")), 4000);
    copyWait = { ok: (t) => { clearTimeout(timer); copyWait = null; ok(t); }, fail: (err) => { clearTimeout(timer); copyWait = null; fail(err); } };
    setTimeout(() => { flush(); send({ type: "copy" }); }, 150); // the page copies on the key; give it a moment
    const blob = text.then((t) => { if (!t) throw new Error("nothing was copied in the browser"); return t; });
    const done = () => say("Copied to your clipboard.");
    const failed = (err) => say("Couldn't copy: " + ((err && err.message) || "this browser refused"));
    if (!window.isSecureContext || !navigator.clipboard) {
      // Over plain HTTP there's no clipboard to write: show the text to copy by hand.
      blob.then((t) => prompt("BrowserPier is on plain HTTP, so it can't write your clipboard. Copy the text from here:", t), failed);
    } else if (window.ClipboardItem && navigator.clipboard.write) {
      navigator.clipboard.write([new ClipboardItem({ "text/plain": blob.then((t) => new Blob([t], { type: "text/plain" })) })]).then(done, failed);
    } else {
      blob.then((t) => navigator.clipboard.writeText(t)).then(done, failed);
    }
  }
  function copied(text) { if (copyWait) copyWait.ok(text || ""); }
  fitBtn.onclick = () => { zoom = 1; panX = panY = 0; applyZoom(); };
  // The stage takes the stream's shape: the profile's screen size may have
  // changed since this browser started.
  video.addEventListener("resize", () => {
    if (video.videoWidth && video.videoHeight) stage.style.setProperty("--ratio", `${video.videoWidth}/${video.videoHeight}`);
  });

  // Mouse.
  video.addEventListener("mousemove", (e) => { const p = toScreen(e.clientX, e.clientY); if (p) push({ t: "move", ...p }); });
  video.addEventListener("mousedown", (e) => {
    if (!control || touch) return;
    e.preventDefault(); keyboard = true; video.focus(); render();
    const p = toScreen(e.clientX, e.clientY); if (p) push({ t: "move", ...p });
    push({ t: "down", b: e.button + 1 });
  });
  video.addEventListener("mouseup", (e) => { if (control && !touch) push({ t: "up", b: e.button + 1 }); });
  video.addEventListener("contextmenu", (e) => { if (control) e.preventDefault(); });
  video.addEventListener("wheel", (e) => {
    if (!control) return;
    e.preventDefault();
    push({ t: "wheel", dy: Math.sign(e.deltaY) * Math.min(5, Math.ceil(Math.abs(e.deltaY) / 100)), dx: Math.sign(e.deltaX) * Math.min(5, Math.ceil(Math.abs(e.deltaX) / 100)) });
  }, { passive: false });

  // Keyboard: named keys by their X keysym, everything printable as a
  // Unicode keysym, so the viewer's own layout is what types.
  const named = {
    Enter: "Return", Backspace: "BackSpace", Tab: "Tab", Delete: "Delete", Insert: "Insert", Home: "Home", End: "End",
    PageUp: "Prior", PageDown: "Next", ArrowLeft: "Left", ArrowRight: "Right", ArrowUp: "Up", ArrowDown: "Down",
    CapsLock: "Caps_Lock", " ": "space", ContextMenu: "Menu",
  };
  const byCode = { ShiftLeft: "Shift_L", ShiftRight: "Shift_R", ControlLeft: "Control_L", ControlRight: "Control_R", AltLeft: "Alt_L", AltRight: "Alt_R",
    MetaLeft: mac ? "Control_L" : "Super_L", MetaRight: mac ? "Control_R" : "Super_R" };
  function keysym(e) {
    if (byCode[e.code]) return byCode[e.code];
    if (named[e.key]) return named[e.key];
    if (/^F([1-9]|1[0-2])$/.test(e.key)) return e.key;
    if ([...e.key].length === 1) { const cp = e.key.codePointAt(0); return "U" + cp.toString(16).toUpperCase().padStart(4, "0"); }
    return null;
  }
  // lift sends what's queued, then lets go of every held key and button.
  function lift() { flush(); send({ type: "lift" }); }
  // Keys held in the browser: a Mac sends no keyup for a key pressed with
  // ⌘ down, so they are let go with ⌘.
  const held = new Set();
  // shortcut is "paste" or "copy" for the person's own shortcuts.
  function shortcut(e) {
    if (!(mac ? e.metaKey : e.ctrlKey) || e.altKey) return "";
    const k = e.code || "";
    if (k === "KeyV" || (!k && e.key.toLowerCase() === "v")) return "paste";
    if (k === "KeyC" || k === "KeyX" || (!k && /^[cx]$/i.test(e.key))) return "copy";
    return "";
  }
  addEventListener("keydown", (e) => {
    // The fixed full screen has no browser Esc of its own: Esc leaves it,
    // and releases the keyboard if it was captured.
    if (e.key === "Escape" && full && !document.fullscreenElement) {
      e.preventDefault();
      leaveFull();
      return;
    }
    if (!control || !keyboard) return;
    if (e.key === "Escape") { e.preventDefault(); lift(); held.clear(); keyboard = false; render(); say("Keyboard released. Click the screen to send it again."); return; }
    const sc = shortcut(e);
    // Paste: not sent as keys and not prevented, so this browser fires its
    // paste event with the person's text.
    if (sc === "paste") { if (!e.repeat) pasteSoon(); return; }
    const k = keysym(e); if (!k) return;
    e.preventDefault(); push({ t: "keydown", k }); held.add(k);
    if (sc === "copy" && !e.repeat) copyFromBrowser();
  });
  addEventListener("keyup", (e) => {
    if (!control || !keyboard || e.key === "Escape") return;
    const k = keysym(e); if (!k) return;
    e.preventDefault();
    // Only what went down goes up: a paste's V was never sent.
    if (held.has(k)) { push({ t: "keyup", k }); held.delete(k); }
    if (mac && (e.code === "MetaLeft" || e.code === "MetaRight")) {
      for (const h of held) push({ t: "keyup", k: h });
      held.clear();
    }
  });
  addEventListener("blur", () => { if (control) lift(); held.clear(); });

  // Phone: tap to click, pinch to zoom, drag to pan when zoomed.
  function applyZoom() { video.style.transform = `translate(${panX}px, ${panY}px) scale(${zoom})`; }
  let touches = null, startDist = 0, startZoom = 1, tapStart = null, panStart = null;
  stage.addEventListener("touchstart", (e) => {
    if (e.touches.length === 2) {
      const [a, b] = e.touches; startDist = Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY); startZoom = zoom; tapStart = null;
    } else if (e.touches.length === 1) {
      const t = e.touches[0]; tapStart = { x: t.clientX, y: t.clientY, at: Date.now() }; panStart = { x: t.clientX - panX, y: t.clientY - panY };
    }
    touches = e.touches.length;
  }, { passive: true });
  stage.addEventListener("touchmove", (e) => {
    e.preventDefault();
    if (e.touches.length === 2 && startDist) {
      const [a, b] = e.touches;
      zoom = Math.max(1, Math.min(4, startZoom * Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY) / startDist));
      applyZoom();
    } else if (e.touches.length === 1 && zoom > 1 && panStart) {
      const t = e.touches[0]; panX = t.clientX - panStart.x; panY = t.clientY - panStart.y; applyZoom();
      if (tapStart && Math.hypot(t.clientX - tapStart.x, t.clientY - tapStart.y) > 10) tapStart = null;
    }
  }, { passive: false });
  stage.addEventListener("touchend", (e) => {
    if (touches === 1 && tapStart && Date.now() - tapStart.at < 500 && control) {
      const p = toScreen(tapStart.x, tapStart.y);
      if (p) { push({ t: "move", ...p }); push({ t: "down", b: 1 }); push({ t: "up", b: 1 }); }
    }
    if (e.touches.length === 0) { touches = 0; startDist = 0; tapStart = null; }
  });

  // failed shows why this view couldn't connect, for good: in the pill,
  // the hint, and a note over the stream, each saying to reload.
  function failed(why) {
    connectError = why;
    render();
    // A refusal's own words end in a full stop already.
    say(why.replace(/[.!?]\s*$/, "") + ". Reload the page to try again.");
    clearTimeout(say.t); // the note stays
  }
  // A network error before the offer's answer (the fetch throwing) is a
  // failure like a refused answer.
  connect().catch((err) => failed("Couldn't connect: " + ((err && err.message) || "network error")));
  poll();
  setInterval(poll, 2000);
  render();
})();
