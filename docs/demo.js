// The hero terminal replays a session: a scripted state machine rendered into
// the two panes, with the key that drove each step shown at the corner. The
// markup already holds a static frame, which is what reduced-motion (and
// no-JS) readers keep.
(() => {
  const leftEl = document.getElementById('pane-left');
  const rightEl = document.getElementById('pane-right');
  const keyEl = document.getElementById('demo-key');
  const demoEl = document.getElementById('demo');
  if (!leftEl || !rightEl || !demoEl) return;
  if (matchMedia('(prefers-reduced-motion: reduce)').matches) return;

  const LINES = 21;
  const SPIN = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];
  const s = (cls, text) => `<span class="t-${cls}">${text}</span>`;

  // ---- state ----

  // Rows carry the tree shape the way the navigator draws it: groups indent
  // their repos, procs get branch rules. `only` limits a row to filter mode.
  const makeRows = () => [
    { id: 'acme', kind: 'group', label: 'acme', indent: ' ' },
    { id: 'web', kind: 'repo', label: 'web', indent: '   ', mark: 'busy' },
    { id: 'dev', kind: 'proc', label: 'npm run dev', indent: '   ', parent: 'web' },
    { id: 'webclaude', kind: 'proc', label: 'claude', indent: '   ', parent: 'web' },
    { id: 'api', kind: 'repo', label: 'api', indent: '   ', mark: 'idle' },
    { id: 'apiclaude', kind: 'proc', label: 'claude', indent: '   ', parent: 'api' },
    { id: 'scrn', kind: 'repo', label: 'scrn', indent: ' ' },
    { id: 'gotest', kind: 'proc', label: 'go test ./...', indent: ' ', parent: 'scrn' },
  ];

  const state = {
    rows: makeRows(),
    cursor: 'dev',
    mode: 'browse', // browse | filter | confirm | shell
    filter: '',
    status: '',
    confirmSubject: '',
    pane: 'vite',
    typed: '', // what has been typed at the shell prompt
    shellDone: false, // the command has run
    shellLive: false, // the cursor block is drawn (shell focused)
    dying: null, // row id wearing the kill spinner
    frame: 0,
  };

  // ---- left pane ----

  const filterMatch = (r) =>
    ['acme', 'web', 'dev', 'webclaude', 'webzsh'].includes(r.id);

  function renderLeft() {
    const lines = [s('title', 'scrn'), ''];
    const rows = state.rows.filter(
      (r) => state.mode !== 'filter' || filterMatch(r)
    );

    for (const r of rows) {
      const sel = r.id === state.cursor;
      const marker = sel ? s('sel', '▸') : ' ';

      let rules = r.indent;
      if (r.kind === 'proc') {
        const sibs = rows.filter((x) => x.parent === r.parent);
        const branch = sibs[sibs.length - 1] === r ? '└─' : '├─';
        rules = r.indent + branch + ' ';
      }

      let cls = 'item';
      if (state.mode === 'filter') cls = sel ? 'sel' : 'dim';
      else if (sel) cls = 'sel';

      let mark = '';
      if (r.mark === 'busy') mark = ' ' + s('busy', SPIN[state.frame % SPIN.length]);
      if (r.mark === 'idle') mark = ' ' + s('dim', '○');
      if (state.dying === r.id) mark = ' ' + s('err', SPIN[state.frame % SPIN.length]);

      lines.push(marker + s('dim', rules) + s(cls, r.label) + mark);
    }

    const hint = hintLines();
    while (lines.length < LINES - hint.length) lines.push('');
    lines.push(...hint);
    return lines.slice(0, LINES).join('\n');
  }

  function hintLines() {
    switch (state.mode) {
      case 'confirm':
        return [
          ' ' + s('warn', `kill ${state.confirmSubject}?`),
          ' ' + s('hint', 'x confirm · any other key'),
          ' ' + s('hint', 'cancels'),
        ];
      case 'filter':
        return [
          ' ' + s('item', '/' + state.filter) + s('cur', ' '),
          ' ' + s('hint', '^n ^p move · enter shell ·'),
          ' ' + s('hint', '^r run · ^a agent · esc'),
        ];
      case 'shell':
        return [
          ' ' + s('warn', 'shell'),
          ' ' + s('hint', 'ctrl+o back to the list'),
        ];
    }
    if (state.status) return [' ' + s('item', state.status)];
    return [
      ' ' + s('hint', '↑↓ move      gg top'),
      ' ' + s('hint', 'G bottom     / find'),
      ' ' + s('hint', 's shell      a agent'),
      ' ' + s('hint', 'r run        enter open'),
      ' ' + s('hint', 'x kill       X kill tree'),
      ' ' + s('hint', 'space fold   - unfold'),
      ' ' + s('hint', '. all        q quit'),
    ];
  }

  // ---- right pane ----

  const panes = {
    vite: () => `
  ${s('mag', 'VITE v6.0.11')}  ${s('green', 'ready in 284 ms')}

  ${s('green', '➜')}  ${s('item', 'Local:')}   ${s('cyan', 'http://localhost:5173/')}
  ${s('green', '➜')}  ${s('dim', 'Network: use --host to expose')}

  ${s('dim', '09:41:07')} ${s('cyan', '[vite]')} ${s('item', 'hmr update')} ${s('dim', '/src/App.tsx')}
  ${s('dim', '09:41:12')} ${s('cyan', '[vite]')} ${s('item', 'hmr update')} ${s('dim', '/src/index.css')}
  ${s('dim', '09:41:19')} ${s('cyan', '[vite]')} ${s('item', 'hmr update')} ${s('dim', '/src/routes/home.tsx')}
  ${s('dim', '09:41:19')} ${s('cyan', '[vite]')} ${s('green', '✓')} ${s('dim', '14 modules transformed')}`,

    claude: () => `
  ${s('mag', '✳')} ${s('head', 'claude')} ${s('dim', '— web')}

  ${s('dim', '>')} ${s('item', 'add a retry to fetchUsers')}

  ${s('busy', SPIN[state.frame % SPIN.length])} ${s('item', 'Editing')} ${s('cyan', 'src/api/users.ts')}

    ${s('dim', 'for (let n = 0; n < 3; n++) {')}
    ${s('dim', '  try { return await get(&quot;/users&quot;) }')}
    ${s('dim', '  catch (e) { if (n == 2) throw e }')}
    ${s('dim', '}')}`,

    apidetail: () => `
  ${s('head', 'api')}

  ${s('label', 'branch ')} ${s('item', 'main')}
  ${s('label', 'plan   ')} ${s('item', '.scrn — 2 entries')}
  ${s('label', 'running')} ${s('item', '1 of 2')}

  ${s('label', 'path   ')} ${s('item', '~/projects/acme/api')}`,

    apilog: () => `
  ${s('dim', '$')} ${s('item', 'go run ./cmd/api')}
  ${s('item', 'api listening on')} ${s('cyan', ':8080')}`,

    webdetail: () => `
  ${s('head', 'web')}

  ${s('label', 'branch ')} ${s('item', 'main')}
  ${s('label', 'plan   ')} ${s('item', 'package.json — dev')}
  ${s('label', 'running')} ${s('item', '1 of 1')}

  ${s('label', 'path   ')} ${s('item', '~/projects/acme/web')}`,

    shell: () => {
      const prompt = s('green', '~/projects/acme/web') + s('dim', ' %') + ' ';
      const lines = ['', '  ' + prompt + s('item', state.typed) +
        (state.shellLive && !state.shellDone ? s('cur', ' ') : '')];
      if (state.shellDone) {
        lines[1] = '  ' + prompt + s('item', 'git status');
        lines.push(
          '  ' + s('item', 'On branch main'),
          '  ' + s('dim', "Your branch is up to date with 'origin/main'."),
          '',
          '  ' + s('item', 'nothing to commit, working tree clean'),
          '  ' + prompt + (state.shellLive ? s('cur', ' ') : '')
        );
      }
      return lines.join('\n');
    },

    gotest: () => `
  ${s('dim', '$')} ${s('item', 'go test ./...')}
  ${s('green', 'ok')}   ${s('item', 'github.com/w0zro/scrn')}   ${s('dim', '0.532s')}`,
  };

  // ---- timeline ----

  const addRow = (id, label, parent, afterId) => {
    const i = state.rows.findIndex((r) => r.id === afterId);
    const after = state.rows[i];
    state.rows.splice(i + 1, 0, {
      id, label, parent, kind: 'proc', indent: after.indent,
    });
  };

  const steps = [
    { t: 2600, do() { /* opening frame */ } },
    { k: 'j', t: 2000, do() { state.cursor = 'webclaude'; state.pane = 'claude'; } },
    { k: 'j', t: 2200, do() { state.cursor = 'api'; state.pane = 'apidetail'; } },
    { k: 'r', t: 2400, do() {
      addRow('apirun', 'go run ./cmd/api', 'api', 'apiclaude');
      state.status = 'started api';
      state.pane = 'apilog';
    } },
    { k: '/', t: 700, do() { state.mode = 'filter'; state.filter = ''; state.status = ''; state.cursor = 'web'; state.pane = 'webdetail'; } },
    { k: 'w', t: 420, do() { state.filter = 'w'; } },
    { k: 'e', t: 420, do() { state.filter = 'we'; } },
    { k: 'b', t: 1100, do() { state.filter = 'web'; } },
    { k: 'enter', t: 1500, do() {
      addRow('webzsh', 'zsh', 'web', 'webclaude');
      state.mode = 'shell';
      state.filter = '';
      state.cursor = 'webzsh';
      state.pane = 'shell';
      state.typed = '';
      state.shellDone = false;
      state.shellLive = true;
    } },
    ...'git status'.split('').map((ch) => (
      { t: 90, do() { state.typed += ch; } }
    )),
    { t: 2300, do() { state.shellDone = true; } },
    { k: 'ctrl+o', t: 1700, do() { state.mode = 'browse'; state.shellLive = false; } },
    { k: 'G', t: 1800, do() { state.cursor = 'gotest'; state.pane = 'gotest'; } },
    { k: 'x', t: 1900, do() { state.mode = 'confirm'; state.confirmSubject = 'go test ./...'; } },
    { k: 'x', t: 1400, do() {
      state.mode = 'browse';
      state.dying = 'gotest';
      state.status = 'killed go test ./...';
    } },
    { t: 2800, do() {
      state.rows = state.rows.filter((r) => r.id !== 'gotest' && r.id !== 'scrn');
      state.dying = null;
      state.cursor = 'webzsh';
      state.pane = 'shell';
    } },
    { t: 100, do() { // reset for the next lap
      Object.assign(state, {
        rows: makeRows(), cursor: 'dev', mode: 'browse', filter: '',
        status: '', pane: 'vite', typed: '', shellDone: false,
        shellLive: false, dying: null,
      });
    } },
  ];

  // ---- runner ----

  function render() {
    leftEl.innerHTML = renderLeft();
    rightEl.innerHTML = panes[state.pane]();
  }

  let keyTimer;
  function showKey(k) {
    if (!keyEl || !k) return;
    keyEl.textContent = k;
    keyEl.classList.add('show');
    clearTimeout(keyTimer);
    keyTimer = setTimeout(() => keyEl.classList.remove('show'), 900);
  }

  let i = 0;
  let stepTimer;
  let spinTimer;
  let running = false;

  function advance() {
    const step = steps[i];
    i = (i + 1) % steps.length;
    showKey(step.k);
    step.do();
    render();
    stepTimer = setTimeout(advance, step.t);
  }

  function start() {
    if (running) return;
    running = true;
    spinTimer = setInterval(() => { state.frame++; render(); }, 90);
    stepTimer = setTimeout(advance, 400);
  }

  function stop() {
    running = false;
    clearInterval(spinTimer);
    clearTimeout(stepTimer);
  }

  // Run only while the terminal is on screen.
  new IntersectionObserver(
    (entries) => (entries[0].isIntersecting ? start() : stop()),
    { threshold: 0.2 }
  ).observe(demoEl);
})();
