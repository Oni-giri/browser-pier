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
  // Whether the profile records, as the page was rendered, then as the
  // profile page's status says, so the hint follows the setting changed
  // elsewhere (the handoff page's status doesn't say).
  let records = "rec" in hint.dataset;
  const note = document.getElementById("note");
  const touch = matchMedia("(pointer: coarse)").matches;
  // On a Mac, ⌘ is sent as Ctrl: the browser runs on Linux, where Super
  // does nothing and Ctrl makes the shortcuts (PROJ-194).
  const mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || "") || (navigator.userAgentData && navigator.userAgentData.platform === "macOS");
  const cmd = mac ? "⌘" : "Ctrl+";
  // The clipboard box (PROJ-194), for the person with control.
  const clipBtn = document.getElementById("clip");
  const clipBox = document.getElementById("clipbox");
  const clipText = document.getElementById("cliptext");
  const clipMsg = document.getElementById("clipmsg");
  // maxClipSet is the most the box sends at once, as BrowserPier caps it.
  const maxClipSet = 10000;

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
    if (clipBtn) clipBtn.hidden = !control;
    if (!control) closeClip();
    stage.classList.toggle("control", control);
    const box = " Clipboard moves text between your clipboard and the browser's.";
    hint.textContent = (control
      ? (touch ? `Tap to click. Pinch to zoom. ${typeBtn.textContent} sends text.` + box
        : (keyboard ? `Your keyboard goes to the browser${mac ? ", ⌘ as Ctrl" : ""}. Esc releases it.` : "Click the screen to send your keyboard to the browser.") + box)
      : "View only. Take control to use the mouse and keyboard.")
      // A profile that records says so in the hint, which full screen
      // keeps (PROJ-23).
      + (records && hint.dataset.recNote ? " " + hint.dataset.recNote : "")
      + (full && !touch ? " Esc leaves full screen." : "");
    if (connectError) hint.textContent = "Reload the page to try again.";
    if (fullBtn) fullBtn.textContent = full ? "Exit full screen" : "Full screen";
  }

  async function poll() {
    try {
      const r = await fetch(statusURL, { cache: "no-store" });
      if (r.redirected || r.status === 401) { location.href = "/login?next=" + encodeURIComponent(location.pathname); return; }
      status = await r.json();
      if (typeof status.record === "boolean") records = status.record;
    } catch (_) { /* keep the last status */ }
    if (control && !status.human_control) { control = false; keyboard = false; closeChooser(); }
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
        render(); poll(); pollChooser();
      }
      if (/^clipboard/.test(m.type)) clipAnswer(m);
      if (m.type === "error") { say(m.msg); poll(); }
    };
    pc.ontrack = (e) => { video.srcObject = e.streams[0] || new MediaStream([e.track]); };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "closed") { who.textContent = "Disconnected"; who.className = "pill"; if (!("handingBack" in document.body.dataset)) setTimeout(() => location.reload(), 3000); }
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
    if (answer.control) pollChooser();
  }

  // A page's file chooser (PROJ-181). While you have control, a page that
  // asks for a file shows this modal: the server renders it, its names
  // escaped, and answers the chooser with what you pick or upload. Nothing
  // else of the page is shown, and no CDP is involved.
  const chooserURL = root.dataset.chooser;
  const chooserBox = document.getElementById("chooser");
  let chooserShown = "", chooserBusy = false;
  function closeChooser() {
    chooserShown = "";
    if (chooserBox && chooserBox.open) chooserBox.close();
  }
  async function pollChooser() {
    if (!chooserURL || !chooserBox || chooserBusy) return;
    if (!control) { closeChooser(); return; }
    let r;
    try { r = await fetch(chooserURL, { cache: "no-store" }); } catch (_) { return; }
    if (r.status === 204 || r.status === 409 || r.status === 403) { closeChooser(); return; }
    if (r.status !== 200 || !control || chooserBusy) return;
    const t = document.createElement("template");
    t.innerHTML = await r.text();
    const form = t.content.querySelector("form.chooser");
    if (!form) return;
    // The same chooser stays as it is, with what you've picked; only the
    // time it has left is new.
    if (chooserBox.open && form.dataset.id === chooserShown) {
      const note = chooserBox.querySelector(".chooser-note"), fresh = form.querySelector(".chooser-note");
      if (note && fresh) note.textContent = fresh.textContent;
      return;
    }
    chooserShown = form.dataset.id;
    chooserBox.replaceChildren(form);
    wireChooser(form);
    if (!chooserBox.open) {
      if (keyboard) lift(); // nothing stays held while the modal is up
      chooserBox.showModal();
    }
  }
  function wireChooser(form) {
    const id = form.dataset.id;
    const choose = form.querySelector("[type=submit]");
    const err = form.querySelector(".chooser-error");
    const progress = form.querySelector(".chooser-progress");
    const device = form.querySelector("[data-device]");
    const picked = () => [...form.querySelectorAll("input[name=file]:checked")].map((i) => i.value);
    const sync = () => {
      const n = picked().length;
      choose.disabled = chooserBusy || n === 0;
      choose.textContent = n > 1 ? `Choose ${n} files` : "Choose";
    };
    const busy = (on) => {
      chooserBusy = on;
      form.querySelectorAll("button, input").forEach((b) => { b.disabled = on; });
      if (!on) sync();
    };
    const fail = (msg) => { err.textContent = msg; err.hidden = false; };
    // done handles the server's answer to a choice, a cancel or an upload.
    const done = (status, text) => {
      if (status >= 200 && status < 300) { closeChooser(); return; }
      if (status === 409) { closeChooser(); say(text || "That file chooser closed."); return; }
      fail(text || "Couldn't answer the page's chooser. Try again.");
    };
    async function post(body) {
      busy(true); err.hidden = true;
      try {
        const r = await fetch(chooserURL, { method: "POST", body });
        done(r.status, (await r.text()).trim());
      } catch (_) { fail("Couldn't reach BrowserPier. Try again."); }
      busy(false);
    }
    form.addEventListener("change", (e) => { if (e.target.name === "file") sync(); });
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const body = new URLSearchParams({ id });
      picked().forEach((n) => body.append("file", n));
      post(body);
    });
    form.querySelector("[data-cancel]").onclick = () => post(new URLSearchParams({ id, cancel: "1" }));
    form.querySelector("[data-upload]").onclick = () => device.click();
    device.onchange = () => {
      const f = device.files[0];
      if (!f) return;
      const body = new FormData();
      body.append("id", id);
      body.append("file", f, f.name);
      const mb = (n) => (n / 1e6).toFixed(n < 1e7 ? 1 : 0) + " MB";
      const text = progress.querySelector(".chooser-progress-text");
      const bar = progress.querySelector(".chooser-bar > span");
      text.textContent = `Uploading ${f.name}…`;
      bar.style.width = "0";
      progress.hidden = false;
      busy(true); err.hidden = true;
      // XMLHttpRequest, for the upload's progress.
      const x = new XMLHttpRequest();
      x.open("POST", chooserURL + "/upload");
      x.upload.onprogress = (e) => {
        if (!e.lengthComputable) return;
        bar.style.width = Math.round(100 * e.loaded / e.total) + "%";
        text.textContent = `Uploading ${f.name}… ${mb(e.loaded)} of ${mb(e.total)}`;
      };
      const end = () => { progress.hidden = true; device.value = ""; busy(false); };
      x.onload = () => { end(); done(x.status, x.responseText.trim()); };
      x.onerror = () => { end(); fail("The upload didn't get through. Try again."); };
      x.send(body);
    };
    sync();
  }
  if (chooserBox) {
    // Esc doesn't close it: the page waits for an answer, so Cancel is.
    chooserBox.addEventListener("cancel", (e) => e.preventDefault());
    setInterval(pollChooser, 1000);
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
  typeBtn.onclick = () => { const text = prompt("Type into the browser"); if (text) send({ type: "text", text }); };

  // The clipboard box (PROJ-194): the browser's clipboard, read into it
  // only when it opens or Refresh is pressed (never polled, so a page
  // can't put text in it on its own), and text sent from it, which stays
  // on the browser's clipboard until the person's control ends. Keys
  // typed in it stay on this page.
  let clipReq = null, clipID = 0;
  // clipEdited: the box holds text the person typed or pasted there and
  // hasn't sent. Opening the box again keeps it; only Refresh replaces it.
  let clipEdited = false;
  // clipEdits counts the person's edits, so a read can tell whether they
  // typed while it was on its way.
  let clipEdits = 0;
  if (clipText) clipText.addEventListener("input", () => { clipEdited = true; clipEdits++; });
  function clipSay(text) { clipMsg.textContent = text; }
  function clipBusy(on) { for (const b of clipBox.querySelectorAll("[data-clip=refresh],[data-clip=send],[data-clip=paste]")) b.disabled = on; }
  // clipAsk sends one request, answered by its id (clipAnswer): done
  // with the answer, or fail, if set, with the error's words.
  function clipAsk(msg, busy, done, fail) {
    const id = ++clipID;
    clearTimeout(clipReq && clipReq.timer);
    clipReq = { id, done, fail: fail || clipSay, timer: setTimeout(() => clipAnswer({ type: "clipboard-error", id, msg: "The browser didn't answer." }), 10000) };
    clipSay(busy); clipBusy(true);
    flush(); // keys pressed before go first
    send({ ...msg, id });
  }
  function clipAnswer(m) {
    if (!clipReq || m.id !== clipReq.id) return;
    const { done, fail, timer } = clipReq;
    clipReq = null; clearTimeout(timer); clipBusy(false);
    if (m.type === "clipboard-error") fail(m.msg || "That didn't work.");
    else done(m);
  }
  // refreshClip reads the browser's clipboard into the box. Refresh's
  // read replaces what is there; the read that opening the box starts
  // (auto) doesn't, if the person typed or pasted while it was on its way.
  function refreshClip(auto) {
    const edits = clipEdits;
    const typedSince = () => auto === true && clipEdits !== edits;
    clipAsk({ type: "clipboard" }, "Reading the browser's clipboard…", (m) => {
      if (typedSince()) { clipSay("Your text isn't sent yet. Refresh replaces it with the browser's clipboard."); return; }
      clipText.value = m.text || ""; clipEdited = false;
      clipSay(m.text ? "The browser's clipboard. Copy puts it on yours." : "The browser's clipboard is empty. Type or paste text here to send it.");
    }, (msg) => {
      // What an earlier read left would pass for the browser's clipboard
      // now: it goes. Text the person typed stays, said to be theirs.
      if (!clipEdited && !typedSince()) clipText.value = "";
      clipSay(clipEdited ? `${msg} The text here is yours, not sent yet.` : msg);
    });
  }
  function openClip() {
    if (!clipBox || !control) return;
    clipBox.hidden = false; clipBtn.setAttribute("aria-expanded", "true");
    // Typing goes to the box, not the browser (a phone's keyboard opens
    // only when the person taps it).
    if (!touch) clipText.focus();
    if (clipEdited) clipSay("Your text isn't sent yet. Refresh replaces it with the browser's clipboard.");
    else refreshClip(true);
  }
  function closeClip() {
    if (!clipBox || clipBox.hidden) return;
    if (clipReq) { clearTimeout(clipReq.timer); clipReq = null; clipBusy(false); }
    clipBox.hidden = true; clipBtn.setAttribute("aria-expanded", "false");
  }
  function sendClip(paste) {
    const text = clipText.value.replace(/\r\n?/g, "\n");
    if (!text) { clipSay("There's no text to send."); return; }
    const n = [...text].length;
    if (n > maxClipSet) { clipSay(`That's ${n.toLocaleString()} characters: at most ${maxClipSet.toLocaleString()} can be sent at once.`); return; }
    clipAsk({ type: "clipboard-set", text, paste }, paste ? "Pasting…" : "Sending…", () => {
      clipEdited = false;
      clipSay(paste ? "Pasted into the page. It stays on the browser's clipboard until you hand back." : `On the browser's clipboard until you hand back: ${cmd}V pastes it there.`);
    });
  }
  async function copyClip() {
    const text = clipText.value;
    if (!text) { clipSay("There's nothing to copy."); return; }
    // Where this browser won't write its clipboard (plain HTTP, or a
    // refusal), the text is selected to copy by hand.
    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      try { await navigator.clipboard.writeText(text); clipSay("Copied"); return; } catch (_) { /* by hand */ }
    }
    clipText.focus(); clipText.select();
    clipSay(touch ? "Selected: copy it from the menu." : `Selected: press ${cmd}C to copy it.`);
  }
  if (clipBtn && clipBox) {
    clipBtn.onclick = () => (clipBox.hidden ? openClip() : closeClip());
    clipBox.addEventListener("click", (e) => {
      const b = e.target.closest("[data-clip]");
      if (!b) return;
      ({ refresh: () => refreshClip(false), close: closeClip, copy: copyClip, send: () => sendClip(false), paste: () => sendClip(true) })[b.dataset.clip]();
    });
  }
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
  function lift() { flush(); send({ type: "lift" }); held.clear(); }
  // releaseKeys gives the keyboard back to this page.
  function releaseKeys() { lift(); keyboard = false; render(); }
  // While the file chooser's modal is up, keys are the modal's.
  const modalUp = () => chooserBox && chooserBox.open;
  // Keys held in the browser, by the physical key, with the keysym sent:
  // a key's keyup can name another keysym than its keydown (Shift let go
  // first: "A" down, "a" up), and must let go of what went down.
  const held = new Map();
  const modifier = (e) => ["Shift", "Control", "Alt", "AltGraph", "Meta", "CapsLock"].includes(e.key);
  addEventListener("keydown", (e) => {
    if (modalUp()) return;
    // The fixed full screen has no browser Esc of its own: Esc leaves it,
    // and releases the keyboard if it was captured.
    // In the clipboard box, keys stay on this page; Esc closes it.
    if (clipBox && clipBox.contains(e.target)) {
      if (e.key === "Escape") { e.preventDefault(); closeClip(); clipBtn.focus(); }
      return;
    }
    if (e.key === "Escape" && full && !document.fullscreenElement) {
      e.preventDefault();
      leaveFull();
      return;
    }
    if (!control || !keyboard) return;
    if (e.key === "Escape") { e.preventDefault(); releaseKeys(); say("Keyboard released. Click the screen to send it again."); return; }
    // Every key goes to the browser, Ctrl+C, X and V included: copy and
    // paste work inside it, on its own clipboard.
    const k = keysym(e); if (!k) return;
    e.preventDefault();
    if (mac && e.metaKey && !modifier(e)) {
      // A Mac sends no keyup for a key pressed with ⌘ down: a tap, so it
      // isn't held (and repeated) in the browser; its repeats are taps.
      push({ t: "keydown", k }); push({ t: "keyup", k });
      return;
    }
    const code = e.code || k;
    if (held.has(code) && held.get(code) !== k) push({ t: "keyup", k: held.get(code) });
    push({ t: "keydown", k }); held.set(code, k);
  });
  addEventListener("keyup", (e) => {
    if (!control || e.key === "Escape") return;
    const code = e.code || keysym(e);
    if (!code) return;
    // A key that went down in the browser always goes up there, wherever
    // the focus has moved since (a button of the clipboard box, say), as
    // the keysym it went down as.
    if (held.has(code)) { push({ t: "keyup", k: held.get(code) }); held.delete(code); }
    // In the box, the key is the page's (Space presses its buttons).
    if (keyboard && !modalUp() && !(clipBox && clipBox.contains(e.target))) e.preventDefault();
  });
  addEventListener("blur", () => { if (control) lift(); });
  // A field of this page (the handoff's note) or anything in the clipboard
  // box takes the keyboard back, letting go of every key held: what is
  // typed or pasted there stays here.
  addEventListener("focusin", (e) => {
    const t = e.target;
    if (keyboard && t !== video && t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || (clipBox && clipBox.contains(t)))) releaseKeys();
  });

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
