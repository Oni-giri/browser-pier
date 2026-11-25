// The recording player (PROJ-23): plays a session's segments in order in
// one <video>, on a timeline of wall-clock time with the gaps marked.
// Vanilla, no build step.
(() => {
  const root = document.querySelector('[data-recording]');
  if (!root) return;
  const rec = JSON.parse(root.dataset.recording || '{}');
  const segs = rec.segments || [];
  const gaps = rec.gaps || [];
  if (!segs.length) return;
  const $ = (id) => document.getElementById(id);
  const video = $('rec-video'), bar = $('rec-bar'), head = $('rec-head'), note = $('rec-note');
  const time = $('rec-time'), playBtn = $('rec-play'), skip = $('rec-skip');
  const t0 = segs[0].start, t1 = segs[segs.length - 1].end, span = Math.max(1, t1 - t0);
  const total = segs.reduce((n, s) => n + (s.end - s.start), 0);
  const pct = (t) => ((Math.min(Math.max(t, t0), t1) - t0) / span) * 100;
  const mark = (cls, from, to, title) => {
    const i = document.createElement('i');
    i.className = cls;
    i.style.left = pct(from) + '%';
    i.style.width = Math.max(pct(to) - pct(from), 0.2) + '%';
    if (title) i.title = title;
    bar.insertBefore(i, head);
  };
  // Between two segments with no gap noted, nothing changed worth keeping.
  for (let k = 1; k < segs.length; k++) {
    if (segs[k].start - segs[k - 1].end > 1500) mark('gap', segs[k - 1].end, segs[k].start, 'Not recorded');
  }
  for (const g of gaps) mark('gap', g.start, g.end, g.label || 'Not recorded');
  for (const s of segs) mark('rec', s.start, s.end);

  const clock = (ms) => new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  const mmss = (ms) => { const s = Math.round(ms / 1000); return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0'); };
  const whyBetween = (from, to) => {
    const g = gaps.filter((x) => x.start < to && x.end > from).sort((a, b) => (b.end - b.start) - (a.end - a.start))[0];
    return g ? g.label : 'Nothing was recorded';
  };

  let cur = -1, speed = 1, wait = null, playing = false, noteTimer = null;
  const showNote = (text, ms) => {
    note.textContent = text;
    note.hidden = false;
    clearTimeout(noteTimer);
    if (ms) noteTimer = setTimeout(() => { note.hidden = true; }, ms);
  };
  const position = () => (cur < 0 ? t0 : Math.min(segs[cur].start + video.currentTime * 1000, segs[cur].end));
  const played = () => {
    let n = 0;
    for (let k = 0; k < cur; k++) n += segs[k].end - segs[k].start;
    return n + (cur < 0 ? 0 : Math.min(video.currentTime * 1000, segs[cur].end - segs[cur].start));
  };
  const update = () => {
    const p = position();
    head.style.left = pct(p) + '%';
    bar.setAttribute('aria-valuenow', String(Math.round(pct(p))));
    time.textContent = clock(p) + ' · ' + mmss(played()) + ' of ' + mmss(total);
    playBtn.textContent = playing ? '❚❚' : '▶';
    playBtn.setAttribute('aria-label', playing ? 'Pause' : 'Play');
  };
  const load = (k, offset) => {
    clearTimeout(wait);
    wait = null;
    cur = k;
    if (video.getAttribute('src') !== segs[k].url) {
      video.src = segs[k].url;
      video.addEventListener('loadedmetadata', () => {
        video.currentTime = offset / 1000;
        video.playbackRate = speed;
        if (playing) video.play().catch(() => {});
        update();
      }, { once: true });
    } else {
      video.currentTime = offset / 1000;
      if (playing) video.play().catch(() => {});
    }
    video.playbackRate = speed;
  };
  // seek goes to wall-clock time t: into the segment holding it, or the
  // next one, saying what the gap was.
  const seek = (t) => {
    let k = segs.findIndex((s) => t < s.end);
    if (k < 0) k = segs.length - 1;
    const offset = Math.max(0, t - segs[k].start);
    if (t < segs[k].start) showNote(whyBetween(t, segs[k].start) + ': not recorded.', 2500);
    load(k, offset);
    update();
  };
  video.addEventListener('ended', () => {
    const next = cur + 1;
    if (next >= segs.length) { playing = false; update(); return; }
    const gap = segs[next].start - segs[cur].end;
    if (gap <= 1500) { load(next, 0); return; }
    const why = whyBetween(segs[cur].end, segs[next].start);
    if (skip.checked) {
      showNote('Skipped ' + mmss(gap) + ' · ' + why, 2500);
      load(next, 0);
      return;
    }
    showNote(why + '. Nothing was recorded from ' + clock(segs[cur].end) + ' to ' + clock(segs[next].start) + '.');
    wait = setTimeout(() => { note.hidden = true; load(next, 0); }, gap / speed);
  });
  video.addEventListener('timeupdate', update);
  const toggle = () => {
    playing = !playing;
    if (playing) {
      if (cur < 0) load(0, 0);
      else if (!wait) video.play().catch(() => {});
    } else {
      video.pause();
    }
    update();
  };
  playBtn.addEventListener('click', toggle);
  for (const r of document.querySelectorAll('input[name=speed]')) {
    r.addEventListener('change', () => { speed = Number(r.value); video.playbackRate = speed; });
  }
  const seekAtPointer = (e) => {
    const box = bar.getBoundingClientRect();
    seek(t0 + ((e.clientX - box.left) / box.width) * span);
  };
  bar.addEventListener('click', seekAtPointer);
  bar.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      e.preventDefault();
      seek(position() + (e.key === 'ArrowRight' ? 5000 : -5000));
    }
  });
  document.addEventListener('keydown', (e) => {
    if (e.target.closest('input, textarea, select, button')) return;
    if (e.key === ' ') { e.preventDefault(); toggle(); }
    const n = ['1', '2', '3', '4'].indexOf(e.key);
    if (n >= 0) {
      const r = document.querySelector('input[name=speed][value="' + [1, 2, 4, 8][n] + '"]');
      r.checked = true;
      r.dispatchEvent(new Event('change'));
    }
  });
  // ?t=<ms since the epoch> opens at that moment, as the agent log links.
  const at = Number(new URLSearchParams(location.search).get('t'));
  if (at > 0) seek(at);
  else load(0, 0);
  update();
})();
