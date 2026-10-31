// Handoff page: the time left, and noticing when the link ends.
"use strict";
(() => {
  const expires = Number(document.body.dataset.expires || 0);
  const timer = document.getElementById("timer");
  if (expires && timer) {
    const tick = () => {
      const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
      timer.textContent = `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")} left`;
      if (left === 0) location.reload();
    };
    tick();
    setInterval(tick, 1000);
  }
  const statusURL = document.body.dataset.status;
  if (statusURL) {
    setInterval(async () => {
      try {
        const r = await fetch(statusURL, { cache: "no-store" });
        if (!r.ok) { location.reload(); return; }
        const s = await r.json();
        if (s.handoff && s.handoff !== "pending") location.reload();
      } catch (_) { /* try again */ }
    }, 3000);
  }
})();
