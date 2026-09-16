(function () {
  if (window.__kvs) return;
  window.__kvs = 1;
  function el(t, c, s) {
    var e = document.createElement(t);
    if (c) e.className = c;
    if (s != null) e.textContent = s;
    return e;
  }
  function at(e, o) { for (var k in o) e.setAttribute(k, o[k]); }
  function act(e, f) {
    e.addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); f(); }
    });
  }
  function tv(n, d) {
    var w = document.querySelector('.monaco-workbench');
    if (!w) return d;
    var v = getComputedStyle(w).getPropertyValue(n).trim();
    return v || d;
  }
  function logout() {
    fetch('/__logout', { credentials: 'include' })
      .then(function () { location.reload(); })
      .catch(function () { location.reload(); });
  }
  function item() {
    var li = el('li', 'action-item icon');
    li.id = '__kvs_out';
    at(li, { role: 'button', 'aria-label': 'Logout', tabindex: '0' });
    li.style.cursor = 'pointer';
    var a = el('a', 'action-label codicon codicon-sign-out');
    at(a, { 'aria-label': 'Logout' });
    li.appendChild(a);
    li.appendChild(el('div', 'active-item-indicator'));
    li.addEventListener('click', logout);
    act(li, logout);
    return li;
  }
  function dialog() {
    if (document.getElementById('__kvs_dlg')) return;
    var o = el('div');
    o.id = '__kvs_dlg';
    o.style.cssText = 'position:fixed;inset:0;z-index:99999;background:rgba(0,0,0,.5);display:flex;align-items:center;justify-content:center';
    var b = el('div');
    b.style.cssText =
      'background:' + tv('--vscode-editorWidget-background', '#252526') +
      ';color:' + tv('--vscode-editorWidget-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-widget-border', '#454545') +
      ';border-radius:6px;padding:20px;min-width:380px;max-width:90vw;box-shadow:0 8px 30px rgba(0,0,0,.5);font-size:13px';
    var t = el('div', null, 'Update');
    t.style.cssText = 'font-size:15px;font-weight:600;margin-bottom:12px';
    var h = el('div', null, 'Current version: ...');
    h.style.cssText = 'color:' + tv('--vscode-descriptionForeground', '#9d9d9d') + ';margin-bottom:10px';
    var i = el('input');
    i.type = 'text';
    i.placeholder = 'Leave empty for current or latest version';
    i.style.cssText =
      'width:100%;box-sizing:border-box' +
      ';background:' + tv('--vscode-input-background', '#3c3c3c') +
      ';color:' + tv('--vscode-input-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-input-border', '#3c3c3c') +
      ';border-radius:2px;padding:6px 8px;font-size:13px;outline:none';
    function btn(s, p) {
      var x = el('button', null, s);
      x.style.cssText = 'padding:5px 12px;border-radius:2px;border:0;font-size:13px;cursor:pointer;' +
        (p ? 'background:' + tv('--vscode-button-background', '#0e639c') + ';color:' + tv('--vscode-button-foreground', '#fff')
           : 'background:' + tv('--vscode-button-secondaryBackground', '#3a3d41') + ';color:' + tv('--vscode-button-secondaryForeground', '#fff'));
      return x;
    }
    var r = el('div');
    r.style.cssText = 'display:flex;justify-content:flex-end;gap:8px;margin-top:16px';
    var c = btn('Cancel'), k = btn('Update & Restart', 1);
    r.appendChild(c); r.appendChild(k);
    var agnLbl = el('div', null, 'Agents (optional restart target)');
    agnLbl.style.cssText = 'color:' + tv('--vscode-descriptionForeground', '#9d9d9d') + ';margin:12px 0 4px;font-size:12px';
    var agnRow = el('div');
    agnRow.style.cssText = 'display:flex;gap:6px;align-items:stretch';
    var agnSel = el('select');
    agnSel.style.cssText =
      'flex:1;min-width:0;box-sizing:border-box;padding:6px 8px;font-size:13px;border-radius:2px' +
      ';background:' + tv('--vscode-input-background', '#3c3c3c') +
      ';color:' + tv('--vscode-input-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-input-border', '#3c3c3c') + ';outline:none';
    agnSel.title = 'Select an agent entry';
    var agnEmpty = el('option', null, ''); agnEmpty.value = '';
    agnSel.appendChild(agnEmpty);
    function fitText(s, n) {
      s = String(s == null ? '' : s);
      if (s.length > n) s = s.slice(0, Math.max(0, n - 3)) + '...';
      return s;
    }
    function fillAgents(entries) {
      var cur = agnSel.value;
      agnSel.innerHTML = '';
      var e0 = el('option', null, ''); e0.value = '';
      agnSel.appendChild(e0);
      (entries || []).forEach(function (e) {
        var full = (e.time ? '[' + e.time + '] ' : '') + e.file;
        var op = el('option', null, fitText(full, 36));
        op.value = e.file;
        op.title = full;
        agnSel.appendChild(op);
      });
      agnSel.value = cur;
      agnSel.title = agnSel.value || 'Select an agent entry';
    }
    var agnRef = btn('Refresh');
    agnRef.style.cssText += ';flex:0 0 auto;min-width:6.5em';
    agnRow.appendChild(agnSel); agnRow.appendChild(agnRef);
    agnRef.addEventListener('click', function () {
      if (agnRef.disabled) return;
      var old = agnRef.textContent;
      agnRef.textContent = '…';
      agnRef.disabled = true;
      fetch('/__agents/entries', { credentials: 'include', signal: AbortSignal.timeout(5000) })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          fillAgents(d);
          agnRef.textContent = '✓';
          setTimeout(function () { agnRef.textContent = old; agnRef.disabled = false; }, 800);
        })
        .catch(function () {
          agnRef.textContent = '✗';
          setTimeout(function () { agnRef.textContent = old; agnRef.disabled = false; }, 800);
        });
    });
    b.appendChild(t); b.appendChild(h); b.appendChild(i);
    b.appendChild(agnLbl); b.appendChild(agnRow);
    b.appendChild(r);
    o.appendChild(b);
    document.body.appendChild(o);
    function close() { o.remove(); }
    function go() {
      var v = i.value.trim();
      var f = agnSel.value;
      var url = '/__restart';
      if (v) url += '?v=' + encodeURIComponent(v);
      if (f) url += (url.indexOf('?') === -1 ? '?' : '&') + 'agent=' + encodeURIComponent(f);
      fetch(url, { credentials: 'include' })
        .catch(function () {})
        .then(function () { setTimeout(function () { location.reload(); }, 1000); });
    }
    k.addEventListener('click', go);
    c.addEventListener('click', close);
    o.addEventListener('click', function (e) { if (e.target === o) close(); });
    i.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); go(); }
      else if (e.key === 'Escape') { e.preventDefault(); close(); }
    });
    fetch('/__version', { credentials: 'include', signal: AbortSignal.timeout(5000) })
      .then(function (r) { return r.text(); })
      .then(function (x) { h.textContent = 'Current version: ' + (x.trim() || 'unknown'); })
      .catch(function () { h.textContent = 'Current version: unknown'; });
    i.focus();
  }
  function agentsDialog() {
    if (document.getElementById('__kvs_dlg')) return;
    var o = el('div');
    o.id = '__kvs_dlg';
    o.style.cssText = 'position:fixed;inset:0;z-index:99999;background:rgba(0,0,0,.5);display:flex;align-items:center;justify-content:center';
    var b = el('div');
    b.style.cssText =
      'background:' + tv('--vscode-editorWidget-background', '#252526') +
      ';color:' + tv('--vscode-editorWidget-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-widget-border', '#454545') +
      ';border-radius:6px;padding:20px;min-width:520px;max-width:90vw;max-height:80vh;overflow:auto;box-shadow:0 8px 30px rgba(0,0,0,.5);font-size:13px';
    var t = el('div', null, 'Agents');
    t.style.cssText = 'font-size:15px;font-weight:600;margin-bottom:4px';
    var st = el('div', null, '');
    st.style.cssText = 'display:flex;align-items:center;color:' + tv('--vscode-descriptionForeground', '#9d9d9d') + ';margin-bottom:10px';
    var lbl = el('div', null, 'Agent host command');
    lbl.style.cssText = 'color:' + tv('--vscode-descriptionForeground', '#9d9d9d') + ';margin-bottom:4px';
    var i = el('textarea');
    i.rows = 5;
    i.spellcheck = false;
    i.placeholder =
      'e.g.  VSC_AGENTS_PORT=7300 /usr/local/bin/vsc-agent --port 7300\n' +
      'Leading VAR=value tokens are applied to the process environment.';
    i.style.cssText =
      'width:100%;box-sizing:border-box;resize:vertical;min-height:90px;margin-bottom:8px' +
      ';background:' + tv('--vscode-input-background', '#3c3c3c') +
      ';color:' + tv('--vscode-input-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-input-border', '#3c3c3c') +
      ';border-radius:2px;padding:6px 8px;font-size:13px;outline:none;font-family:monospace;line-height:1.5';
    var hint = el('div', null, 'Start/Restart replace the configured command with the value above (kvs.ini unchanged); Stop takes no command.');
    hint.style.cssText = 'color:' + tv('--vscode-descriptionForeground', '#9d9d9d') + ';font-size:12px;margin-bottom:12px';
    function btn(s, p) {
      var x = el('button', null, s);
      x.style.cssText = 'padding:5px 12px;border-radius:2px;border:0;font-size:13px;cursor:pointer;' +
        (p ? 'background:' + tv('--vscode-button-background', '#0e639c') + ';color:' + tv('--vscode-button-foreground', '#fff')
           : 'background:' + tv('--vscode-button-secondaryBackground', '#3a3d41') + ';color:' + tv('--vscode-button-secondaryForeground', '#fff'));
      return x;
    }
    function dis(x, d) {
      x.disabled = !!d;
      x.style.opacity = d ? '0.5' : '1';
      x.style.cursor = d ? 'not-allowed' : 'pointer';
    }
    var r1 = el('div');
    r1.style.cssText = 'display:flex;justify-content:flex-end;gap:8px';
    var bStart = btn('Start'), bRestart = btn('Restart'), bStop = btn('Stop');
    var actions = [['Patch', 'patch_cmd'], ['Revert', 'patch_rev']];
    var bUpdate = btn('Update');
    var actBtns = actions.map(function (a) { return btn(a[0]); });
    var leftBtns = [bUpdate].concat(actBtns);
    leftBtns.forEach(function (x, k) {
      if (k === leftBtns.length - 1) x.style.cssText += ';margin-right:auto';
      r1.appendChild(x);
    });
    r1.appendChild(bStart); r1.appendChild(bRestart); r1.appendChild(bStop);
    var spin = el('span');
    spin.style.cssText =
      'display:none;margin-left:auto;width:10px;height:10px;vertical-align:-1px' +
      ';border:2px solid ' + tv('--vscode-descriptionForeground', '#9d9d9d') +
      ';border-top-color:transparent;border-radius:50%';
    var stText = document.createTextNode('');
    st.appendChild(stText);
    st.appendChild(spin);
    var resText = document.createTextNode('');
    var res = el('span');
    res.style.cssText = 'display:inline-block;max-width:340px;vertical-align:middle;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;margin-left:auto';
    res.appendChild(resText);
    st.appendChild(res);
    function setStatus(msg) { stText.nodeValue = msg; }
    function setResult(msg) {
      resText.nodeValue = msg;
      res.style.display = msg ? 'inline-block' : 'none';
      res.title = msg || '';
    }
    function setSpin(on) {
      spin.style.display = on ? 'inline-block' : 'none';
      spin.style.animation = on ? 'kvs-spin 1s linear infinite' : '';
    }
    if (!document.getElementById('__kvs_spin_style')) {
      var ks = document.createElement('style');
      ks.id = '__kvs_spin_style';
      ks.textContent = '@keyframes kvs-spin{to{transform:rotate(360deg)}}';
      document.head.appendChild(ks);
    }
    function lockAll() {
      dis(bStart, true); dis(bRestart, true); dis(bStop, true);
      dis(bUpdate, true);
      actBtns.forEach(function (x) { dis(x, true); });
    }
    function req(method, url, body, okLabel, errPrefix) {
      lockAll();
      setSpin(true);
      var opt = { method: method, credentials: 'include' };
      if (body !== undefined && body !== null) {
        opt.headers = { 'Content-Type': 'application/json' };
        opt.body = JSON.stringify(body);
      }
      fetch(url, opt)
        .then(function (res) { return res.ok ? res.text() : res.text().then(function (t) { throw new Error(t); }); })
        .then(function () { setResult(okLabel); })
        .catch(function (e) { setResult(errPrefix + (e && e.message ? e.message : '')); })
        .then(function () { setSpin(false); refresh(); });
    }
    function post(url, body, okLabel, errPrefix) {
      req('POST', url, body, okLabel, errPrefix);
    }
    bStart.addEventListener('click', function () {
      post('/__agents/start', { command: i.value.trim() }, 'Start Success', 'Start Error: ');
    });
    bRestart.addEventListener('click', function () {
      post('/__agents/restart', { command: i.value.trim() }, 'Restart Success', 'Restart Error: ');
    });
    bStop.addEventListener('click', function () {
      post('/__agents/stop', undefined, 'Stop Success', 'Stop Error: ');
    });
    actions.forEach(function (a, k) {
      actBtns[k].addEventListener('click', function () {
        req('GET', '/__agents/action/' + a[1], undefined, a[0] + ' Success', a[0] + ' Error: ');
      });
    });
    bUpdate.addEventListener('click', function () {
      lockAll();
      dis(bUpdate, true);
      setSpin(true);
      setResult('');
      fetch('/__agents/entries', { credentials: 'include', signal: AbortSignal.timeout(5000) })
        .then(function (r) { return r.ok ? r.json() : []; })
        .catch(function () { return []; })
        .then(function (d) {
          var f = d && d.length ? d[0].file : '';
          if (!f) { setResult('No Agent File'); return; }
          return fetch('/__restart?agent=' + encodeURIComponent(f), { credentials: 'include' })
            .then(function () {
              setResult('Update Success');
              setTimeout(function () { location.reload(); }, 1000);
            })
            .catch(function (e) { setResult('Update Error: ' + (e && e.message ? e.message : '')); });
        })
        .then(function () { setSpin(false); dis(bUpdate, false); refresh(); });
    });
    function refresh() {
      fetch('/__agents/status', { credentials: 'include', signal: AbortSignal.timeout(5000) })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          i.value = d.command || '';
          setStatus('Status: ' + (d.running ? 'running' : 'stopped'));
          dis(bStart, !!d.running);
          dis(bRestart, !d.running);
          dis(bStop, !d.running);
          dis(bUpdate, false);
          actBtns.forEach(function (x) { dis(x, false); });
        })
        .catch(function () {
          setStatus('Status: unknown');
          dis(bUpdate, false);
          actBtns.forEach(function (x) { dis(x, false); });
        });
    }
    b.appendChild(t); b.appendChild(st); b.appendChild(lbl);
    b.appendChild(i); b.appendChild(hint); b.appendChild(r1);
    setResult('');
    o.appendChild(b);
    document.body.appendChild(o);
    function close() { o.remove(); }
    o.addEventListener('click', function (e) { if (e.target === o) close(); });
    o.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { e.preventDefault(); close(); }
    });
    refresh();
  }
  function agents() {
    var li = el('li', 'action-item');
    li.id = '__kvs_agn';
    at(li, { role: 'presentation', tabindex: '-1' });
    li.style.cursor = 'pointer';
    var a = el('a', 'action-menu-item');
    at(a, { role: 'menuitem', tabindex: '0' });
    a.style.color = 'var(--vscode-menu-foreground)';
    var c = el('span', 'menu-item-check codicon codicon-menu-selection');
    at(c, { role: 'none' });
    var l = el('span', 'action-label', 'Agents');
    at(l, { 'aria-label': 'Agents' });
    a.appendChild(c); a.appendChild(l); li.appendChild(a);
    a_hover(li);
    a.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      agentsDialog();
    });
    act(li, agentsDialog);
    return li;
  }
  function upd() {
    var li = el('li', 'action-item');
    li.id = '__kvs_upd';
    at(li, { role: 'presentation', tabindex: '-1' });
    li.style.cursor = 'pointer';
    var a = el('a', 'action-menu-item');
    at(a, { role: 'menuitem', tabindex: '0' });
    var c = el('span', 'menu-item-check codicon codicon-menu-selection');
    at(c, { role: 'none' });
    var l = el('span', 'action-label', 'Update');
    at(l, { 'aria-label': 'Update' });
    a.appendChild(c); a.appendChild(l); li.appendChild(a);
    a_hover(li);
    a.addEventListener('click', dialog);
    act(li, dialog);
    return li;
  }
  var LANGS = [
    ['',      'Default'],
    ['cs',    'Čeština'],
    ['de',    'Deutsch'],
    ['en',    'English'],
    ['es',    'Español'],
    ['fr',    'Français'],
    ['it',    'Italiano'],
    ['ja',    '日本語'],
    ['ko',    '한국어'],
    ['pl',    'Polski'],
    ['pt-br', 'Português'],
    ['ru',    'Русский'],
    ['tr',    'Türkçe'],
    ['zh-cn', '简体中文'],
    ['zh-tw', '繁體中文']
  ];
  function curLocale() {
    return (document.cookie.match(/(?:^|;\s*)vscode\.nls\.locale=([^;]*)/) || [])[1] || '';
  }
  function a_hover(el) {
    el.addEventListener('mouseenter', function () {
      el.style.background = tv('--vscode-list-hoverBackground', '#2a2d2e');
    });
    el.addEventListener('mouseleave', function () {
      el.style.background = 'transparent';
    });
  }
  function lsub(li) {
    var sub = document.getElementById('__kvs_lsub');
    if (sub) sub.remove();
    var cur = curLocale();
    sub = el('div', '__kvs_lsub');
    sub.id = '__kvs_lsub';
    at(sub, { role: 'presentation' });
    sub.style.cssText =
      'position:fixed;display:none;z-index:99999;max-height:80vh;overflow:auto' +
      ';background:' + tv('--vscode-menu-background', '#252526') +
      ';color:' + tv('--vscode-menu-foreground', '#ccc') +
      ';border:1px solid ' + tv('--vscode-widget-border', '#454545') +
      ';border-radius:' + tv('--vscode-corner-radius-large', '6px') +
      ';box-shadow:0 8px 30px rgba(0,0,0,.5);font-size:13px;padding:4px 0';
    var ac = el('div');
    ac.style.cssText = 'padding:0';
    for (var i = 0; i < LANGS.length; i++) {
      (function (lc, ln) {
        var row = el('div');
        row.style.cssText = 'display:flex;align-items:center;gap:8px;padding:4px 14px;cursor:pointer;white-space:nowrap;color:' + tv('--vscode-menu-foreground', '#ccc');
        var lbl = el('span', null, ln);
        lbl.style.cssText = 'flex:1';
        a_hover(row);
        row.appendChild(lbl);
        if (lc === cur) {
          var chk = el('span', 'codicon codicon-check');
          at(chk, { role: 'none' });
          chk.style.cssText = 'color:' + tv('--vscode-menu-selectionForeground', tv('--vscode-list-activeSelectionForeground', '#fff'));
          row.appendChild(chk);
        }
        function sel() {
          if (lc === '') {
            document.cookie = 'vscode.nls.locale=;expires=Thu, 01 Jan 1970 00:00:00 GMT;path=/';
          } else {
            document.cookie = 'vscode.nls.locale=' + encodeURIComponent(lc) + ';path=/';
          }
          location.reload();
        }
        row.addEventListener('click', function (e) { e.stopPropagation(); sel(); });
        ac.appendChild(row);
      })(LANGS[i][0], LANGS[i][1]);
    }
    sub.appendChild(ac);
    document.body.appendChild(sub);
    var r = li.getBoundingClientRect();
    sub.style.display = 'block';
    var sr = sub.getBoundingClientRect();
    var x = r.right;
    var y = r.top;
    if (x + sr.width > window.innerWidth) x = r.left - sr.width;
    if (y + sr.height > window.innerHeight) y = Math.max(0, window.innerHeight - sr.height);
    sub.style.left = x + 'px';
    sub.style.top = y + 'px';
    sub.addEventListener('mouseenter', function () {
      clearTimeout(window.__kvs_lhover);
      sub.style.display = 'block';
    });
    sub.addEventListener('mouseleave', function () {
      window.__kvs_lhover = setTimeout(function () { sub.style.display = 'none'; }, 800);
    });
    return sub;
  }
  function lang() {
    var li = el('li', 'action-item');
    li.id = '__kvs_lang';
    at(li, { role: 'presentation', tabindex: '-1' });
    li.style.cursor = 'pointer';
    var a = el('a', 'action-menu-item');
    at(a, { role: 'menuitem', tabindex: '0' });
    a.style.color = 'var(--vscode-menu-foreground)';
    var c = el('span', 'menu-item-check codicon codicon-menu-selection');
    at(c, { role: 'none' });
    var l = el('span', 'action-label', 'Language');
    at(l, { 'aria-label': 'Language' });
    var ind = el('span', 'submenu-indicator codicon codicon-menu-submenu');
    at(ind, { 'aria-hidden': 'true' });
    a.appendChild(c); a.appendChild(l); a.appendChild(ind); li.appendChild(a);
    a_hover(li);
    var hoverTimer = null;
    function open() {
      clearTimeout(window.__kvs_lhover);
      lsub(li);
    }
    function close() {
      window.__kvs_lhover = setTimeout(function () {
        var sub = document.getElementById('__kvs_lsub');
        if (sub) sub.style.display = 'none';
      }, 800);
    }
    li.addEventListener('mouseenter', open);
    li.addEventListener('mouseleave', close);
    a.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      var sub = document.getElementById('__kvs_lsub');
      if (sub && sub.style.display === 'block') { sub.style.display = 'none'; }
      else open();
    });
    return li;
  }
  function sync() {
    var tb = document.querySelector('.activitybar ul.actions-container[role="toolbar"]');
    if (tb && !tb.querySelector('#__kvs_out')) tb.insertBefore(item(), tb.firstChild);
    var btns = document.querySelectorAll('.menubar-menu-button');
    var help = null;
    for (var i = btns.length - 1; i >= 0; i--) {
      if ((btns[i].textContent || '').trim()) { help = btns[i]; break; }
    }
    var open = document.querySelector('.menubar-menu-button.open');
    var menu = open && open.querySelector('.monaco-menu');
    var items = menu && menu.querySelectorAll(':scope > .monaco-action-bar > .actions-container > .action-item');
    var last = items && items[items.length - 1];
    var lasa = last && last.querySelector(':scope > a');
    if (lasa && lasa.className.indexOf('monaco-submenu-item') !== -1) {
      var sub = last.querySelector('.monaco-menu');
      items = sub ? sub.querySelectorAll(':scope > .monaco-action-bar > .actions-container > .action-item') : null;
    } else if (open !== help) {
      items = null;
    }
    var ab = null;
    if (items) {
      for (var i = items.length - 1; i >= 0; i--) {
        if (items[i].id !== '__kvs_upd' && items[i].id !== '__kvs_lang' && items[i].id !== '__kvs_agn') { ab = items[i]; break; }
      }
    }
    if (!ab) return;
    var nn = ab.nextElementSibling;
    if (!(nn && nn.id === '__kvs_lang')) {
      ab.parentNode.insertBefore(lang(), ab.nextSibling);
    }
    var langEl = ab.parentNode.querySelector('#__kvs_lang');
    if (langEl) {
      var an = langEl.nextElementSibling;
      if (!(an && an.id === '__kvs_agn')) {
        langEl.parentNode.insertBefore(agents(), langEl.nextSibling);
      }
      var agnEl = langEl.nextElementSibling;
      if (agnEl && agnEl.id === '__kvs_agn') {
        var un = agnEl.nextElementSibling;
        if (!(un && un.id === '__kvs_upd')) {
          agnEl.parentNode.insertBefore(upd(), agnEl.nextSibling);
        }
      }
    } else {
      var n2 = ab.nextElementSibling;
      if (!(n2 && n2.id === '__kvs_upd')) {
        ab.parentNode.insertBefore(upd(), ab.nextSibling);
      }
    }
  }
  function start() {
    sync();
    if (window.__kvs_obs) return;
    window.__kvs_obs = new MutationObserver(sync);
    window.__kvs_obs.observe(document.documentElement, { childList: true, subtree: true });
  }
  if (document.documentElement) start();
  else document.addEventListener('DOMContentLoaded', start);
})();
