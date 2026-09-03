// Space, behind the page: a field of stars on a canvas, streaming slowly
// past the way they would from a ship under way, the near ones faster than
// the far. A few of the bright ones breathe. The bridge clock in the header
// keeps ship time, in UTC. Nothing here moves for a reader who asked for
// less motion: the stars are drawn once and stay.
(() => {
  const still = matchMedia('(prefers-reduced-motion: reduce)').matches;

  const canvas = document.getElementById('space');
  if (canvas && canvas.getContext) {
    const ctx = canvas.getContext('2d');
    let W = 0, H = 0, dpr = 1, stars = [];

    // Three depths: a haze of faint far stars, a middle field, and a few
    // near ones that are bright and colored — the blue-white and the
    // amber of real ones, and one or two of conn's own violet.
    const TINTS = ['#dfe5f0', '#dfe5f0', '#dfe5f0', '#b2daff', '#ffe2b0', '#b9a7ff'];
    const seed = () => {
      stars = [];
      const n = Math.round((W * H) / 2200);
      for (let i = 0; i < n; i++) {
        const depth = Math.random();
        const near = depth > 0.92, mid = depth > 0.6;
        stars.push({
          x: Math.random() * W,
          y: Math.random() * H,
          r: near ? 1.1 + Math.random() * 0.8 : mid ? 0.7 + Math.random() * 0.4 : 0.35 + Math.random() * 0.3,
          a: near ? 0.85 : mid ? 0.55 : 0.28,
          v: near ? 0.035 : mid ? 0.014 : 0.005,
          tint: near ? TINTS[Math.floor(Math.random() * TINTS.length)] : '#dfe5f0',
          tw: near && Math.random() < 0.5 ? 1 + Math.random() * 2 : 0,
          ph: Math.random() * Math.PI * 2,
        });
      }
    };

    const size = () => {
      dpr = Math.min(devicePixelRatio || 1, 2);
      W = innerWidth; H = innerHeight;
      canvas.width = W * dpr; canvas.height = H * dpr;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      seed();
      draw(0);
    };

    function draw(t) {
      ctx.clearRect(0, 0, W, H);
      for (const s of stars) {
        let a = s.a;
        if (s.tw) a *= 0.7 + 0.3 * Math.sin(t / (600 * s.tw) + s.ph);
        ctx.globalAlpha = a;
        ctx.fillStyle = s.tint;
        ctx.beginPath();
        ctx.arc(s.x, s.y, s.r, 0, Math.PI * 2);
        ctx.fill();
      }
      ctx.globalAlpha = 1;
    }

    let last = 0, raf = 0;
    const step = t => {
      const dt = Math.min(t - last, 50); last = t;
      // Under way: the field drifts down and a little to the left, the
      // near stars faster, each wrapping to the top when it leaves.
      for (const s of stars) {
        s.y += s.v * dt; s.x -= s.v * dt * 0.18;
        if (s.y > H + 2) { s.y = -2; s.x = Math.random() * W; }
        if (s.x < -2) s.x = W + 2;
      }
      draw(t);
      raf = requestAnimationFrame(step);
    };
    const go = () => { if (!raf && !still) raf = requestAnimationFrame(t => { last = t; step(t); }); };
    const stop = () => { cancelAnimationFrame(raf); raf = 0; };

    size();
    addEventListener('resize', size);
    document.addEventListener('visibilitychange', () => document.hidden ? stop() : go());
    go();
  }

  const clock = document.querySelector('[data-clock]');
  if (clock) {
    const tick = () => {
      const d = new Date();
      const p = n => String(n).padStart(2, '0');
      clock.textContent = 'UTC ' + p(d.getUTCHours()) + ':' + p(d.getUTCMinutes()) + ':' + p(d.getUTCSeconds());
    };
    tick();
    if (!still) setInterval(tick, 1000);
  }
})();
