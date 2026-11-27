// Handoff page: the time left, and noticing when the link ends.
//
// Once the link's time is up, or its end is under way (data-ending, the
// server's "Handing back…" page), the page shows that it is handing back
// and asks the link's status, a little less often each time, until the
// handoff is over: it never reloads in a loop while the browser is being
// handed back, which can take a while.
"use strict";
(() => {
  const body = document.body;
  const expires = Number(body.dataset.expires || 0);
  const timer = document.getElementById("timer");
  const statusURL = body.dataset.ending || body.dataset.status || body.dataset.endingStatus;
  let ending = false;

  // pollEnd asks the status with backoff, and reloads once it is final.
  const pollEnd = (delay) => {
    setTimeout(async () => {
      try {
        const r = await fetch(statusURL, { cache: "no-store" });
        if (r.ok) {
          const s = await r.json();
          if (s.handoff && s.handoff !== "pending") { location.reload(); return; }
        } else if (r.status === 404) { location.reload(); return; }
      } catch (_) { /* try again */ }
      pollEnd(Math.min(delay * 1.5, 15000));
    }, delay);
  };

  // showEnding turns the page into the "Handing back…" state, with no
  // controls left, and waits for the end.
  const showEnding = () => {
    if (ending) return;
    ending = true;
    if (timer) timer.textContent = "Handing back…";
    const main = document.querySelector("main.ho, .ho-viewer");
    if (main && !main.querySelector("[data-handing-back]")) {
      const sec = document.createElement("section");
      sec.className = "cell ho-end";
      sec.dataset.handingBack = "";
      const h = document.createElement("h1");
      h.textContent = "Handing back…";
      const p = document.createElement("p");
      p.textContent = "The browser is going back to the agent. This page updates by itself once it has.";
      sec.append(h, p);
      const wrap = document.createElement("main");
      wrap.className = "ho";
      wrap.append(sec);
      main.replaceWith(wrap);
    }
    if (statusURL) pollEnd(2000);
  };

  if (body.dataset.ending) {
    showEnding();
    return;
  }
  if (expires && timer) {
    if (expires <= Date.now()) {
      // Past its time on load: never reload, show the end instead.
      showEnding();
      return;
    }
    const tick = () => {
      if (ending) return;
      const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
      timer.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")} left`;
      if (left === 0) showEnding();
    };
    tick();
    setInterval(tick, 1000);
  }
  if (body.dataset.status) {
    const poll = setInterval(async () => {
      if (ending) { clearInterval(poll); return; }
      try {
        const r = await fetch(body.dataset.status, { cache: "no-store" });
        if (!r.ok) { location.reload(); return; }
        const s = await r.json();
        if (s.ending) { clearInterval(poll); showEnding(); return; }
        if (s.handoff && s.handoff !== "pending") location.reload();
      } catch (_) { /* try again */ }
    }, 3000);
  }
})();
