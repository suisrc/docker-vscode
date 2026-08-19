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
  // Read a --vscode-* theme variable from .monaco-workbench (the variables are
  // scoped there, not on body), falling back to d. Re-read each time so the
  // dialog follows theme changes.
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

    b.appendChild(t); b.appendChild(h); b.appendChild(i); b.appendChild(r);
    o.appendChild(b);
    document.body.appendChild(o);

    function close() { o.remove(); }
    function go() {
      var v = i.value.trim();
      fetch(v ? '/__restart?v=' + encodeURIComponent(v) : '/__restart', { credentials: 'include' })
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

  // Build the submenu DOM and attach it to document.body (NOT inside the <li>,
  // because the <li>'s ancestors have overflow:hidden which would clip it).
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

    // Position the submenu to the right of the Language menu item
    var r = li.getBoundingClientRect();
    // Temporarily show to measure
    sub.style.display = 'block';
    var sr = sub.getBoundingClientRect();
    var x = r.right;
    var y = r.top;
    if (x + sr.width > window.innerWidth) x = r.left - sr.width;
    if (y + sr.height > window.innerHeight) y = Math.max(0, window.innerHeight - sr.height);
    sub.style.left = x + 'px';
    sub.style.top = y + 'px';

    // Keep submenu alive when the mouse is over it (it's on body, not in li)
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
        if (items[i].id !== '__kvs_upd' && items[i].id !== '__kvs_lang') { ab = items[i]; break; }
      }
    }
    if (!ab) return;
    // Insert language item right after the last real menu item
    var nn = ab.nextElementSibling;
    if (!(nn && nn.id === '__kvs_lang')) {
      ab.parentNode.insertBefore(lang(), ab.nextSibling);
    }
    // Insert update item after the language item (if not already there)
    var langEl = ab.parentNode.querySelector('#__kvs_lang');
    if (langEl) {
      var un = langEl.nextElementSibling;
      if (!(un && un.id === '__kvs_upd')) {
        langEl.parentNode.insertBefore(upd(), langEl.nextSibling);
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
