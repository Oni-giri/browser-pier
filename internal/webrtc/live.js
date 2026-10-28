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
  const video = document.getElementById("screen");
  const stage = document.getElementById("stage");
  const who = document.getElementById("who");
  const takeBtn = document.getElementById("take");
  const typeBtn = document.getElementById("type");
  const fitBtn = document.getElementById("fit");
  const hint = document.getElementById("hint");
  const note = document.getElementById("note");
  const touch = matchMedia("(pointer: coarse)").matches;

  let dc = null;
  let control = false;
  let keyboard = false; // keys go to the browser (released with Esc)
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
    who.textContent = label;
    who.className = "pill " + cls;
    // While it waits for help the agent is paused, so a person can always take over.
    const agentDriving = s === "running" && !!(status.in_use_by && status.in_use_by.length);
    if (takeBtn) {
      takeBtn.textContent = control ? "Hand back" : "Take control";
      if (takeBtn.classList.contains("btn")) takeBtn.classList.toggle("blue", !control);
      takeBtn.disabled = !dc || dc.readyState !== "open" || (!control && (agentDriving || status.human_control || (s !== "running" && s !== "needs_help")));
    }
    typeBtn.hidden = !(control && touch);
    fitBtn.hidden = !touch;
    stage.classList.toggle("control", control);
    hint.textContent = control
      ? (touch ? `Tap to click. Pinch to zoom. ${typeBtn.textContent} sends text.` : keyboard ? "Your keyboard goes to the browser. Esc releases it." : "Click the screen to send your keyboard to the browser.")
      : "View only. Take control to use the mouse and keyboard.";
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

  async function connect() {
    const pc = new RTCPeerConnection({ iceServers: [] });
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
      if (m.type === "error") { say(m.msg); poll(); }
    };
    pc.ontrack = (e) => { video.srcObject = e.streams[0] || new MediaStream([e.track]); };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "closed") { who.textContent = "Disconnected"; who.className = "pill"; setTimeout(() => location.reload(), 3000); }
    };
    await pc.setLocalDescription(await pc.createOffer());
    await new Promise((done) => {
      if (pc.iceGatheringState === "complete") return done();
      pc.onicegatheringstatechange = () => pc.iceGatheringState === "complete" && done();
      setTimeout(done, 3000);
    });
    const r = await fetch(offerURL, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ sdp: pc.localDescription.sdp }),
    });
    if (!r.ok) { who.textContent = (await r.text()).trim() || "Couldn't connect"; return; }
    const answer = await r.json();
    await pc.setRemoteDescription({ type: "answer", sdp: answer.sdp });
    if (answer.control) { control = true; keyboard = !touch; status.human_control = true; render(); }
  }

  if (takeBtn) takeBtn.onclick = () => send({ type: control ? "release" : "take" });
  typeBtn.onclick = () => { const text = prompt("Type into the browser"); if (text) send({ type: "text", text }); };
  fitBtn.onclick = () => { zoom = 1; panX = panY = 0; applyZoom(); };

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
  const byCode = { ShiftLeft: "Shift_L", ShiftRight: "Shift_R", ControlLeft: "Control_L", ControlRight: "Control_R", AltLeft: "Alt_L", AltRight: "Alt_R", MetaLeft: "Super_L", MetaRight: "Super_R" };
  function keysym(e) {
    if (byCode[e.code]) return byCode[e.code];
    if (named[e.key]) return named[e.key];
    if (/^F([1-9]|1[0-2])$/.test(e.key)) return e.key;
    if ([...e.key].length === 1) { const cp = e.key.codePointAt(0); return "U" + cp.toString(16).toUpperCase().padStart(4, "0"); }
    return null;
  }
  // lift sends what's queued, then lets go of every held key and button.
  function lift() { flush(); send({ type: "lift" }); }
  addEventListener("keydown", (e) => {
    if (!control || !keyboard) return;
    if (e.key === "Escape") { e.preventDefault(); lift(); keyboard = false; render(); say("Keyboard released. Click the screen to send it again."); return; }
    const k = keysym(e); if (!k) return;
    e.preventDefault(); push({ t: "keydown", k });
  });
  addEventListener("keyup", (e) => {
    if (!control || !keyboard || e.key === "Escape") return;
    const k = keysym(e); if (!k) return;
    e.preventDefault(); push({ t: "keyup", k });
  });
  addEventListener("blur", () => { if (control) lift(); });

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

  connect().catch((err) => { who.textContent = "Couldn't connect: " + err.message; });
  poll();
  setInterval(poll, 2000);
  render();
})();
