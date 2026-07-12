/**
 * logout.wtp.js — Selkies Webtop Logout
 *
 * Adds a "退出系统" sidebar entry + confirm dialog.
 * GET /__logout on confirm, then reload.
 * Usage: <script src="logout.wtp.js"></script> before </body>
 */
(function () {
  'use strict';

  /* 1. Styles */
  document.head.appendChild(Object.assign(document.createElement('style'), {
    textContent: [
      '.logout-overlay{position:fixed;inset:0;z-index:10000;background:rgba(0,0,0,.6);display:flex;align-items:center;justify-content:center}',
      '.logout-overlay.hidden{display:none}',
      '.logout-dialog{background:var(--sidebar-bg,#282c34);border:1px solid var(--sidebar-border,#3a3f47);border-radius:14px;padding:28px 32px;max-width:380px;width:90%;box-shadow:0 10px 40px rgba(0,0,0,.5);text-align:center}',
      '.logout-title{color:var(--sidebar-text,#abb2bf);font-size:1.2em;margin:0 0 12px}',
      '.logout-message{color:var(--sidebar-text,#abb2bf);font-size:.95em;margin:0 0 24px}',
      '.logout-actions{display:flex;gap:12px;justify-content:center}',
      '.logout-btn{padding:10px 24px;border-radius:8px;border:1px solid var(--item-border,#5a606b);font-size:.95em;font-weight:600;cursor:pointer;transition:opacity .15s,transform .1s}',
      '.logout-btn:active{transform:scale(.96)}',
      '.logout-btn-cancel{background:var(--input-bg,#454b54);color:var(--sidebar-text,#abb2bf)}',
      '.logout-btn-cancel:hover{background:var(--section-bg,#3a3f47)}',
      '.logout-btn-confirm{background:var(--button-bg,#61dafb);color:var(--button-text,#282c34);border-color:var(--button-bg,#61dafb)}',
      '.logout-btn-confirm:hover{background:var(--button-hover-bg,#a4d9f5)}',
      '.logout-btn-confirm.loading{opacity:.6;pointer-events:none}',
      '.sidebar-logout-section .sidebar-section-header{cursor:pointer}',
      '.sidebar-logout-section .sidebar-section-header h3{color:var(--sidebar-header-color,#61dafb)}'
    ].join('')
  }));

  /* 2. Overlay dialog */
  var overlay = document.createElement('div');
  overlay.id = 'logout-overlay';
  overlay.className = 'logout-overlay hidden';
  overlay.innerHTML =
    '<div class="logout-dialog">' +
      '<h2 class="logout-title">退出系统</h2>' +
      '<p class="logout-message">确定要退出当前会话吗？</p>' +
      '<div class="logout-actions">' +
        '<button class="logout-btn logout-btn-cancel">取消</button>' +
        '<button class="logout-btn logout-btn-confirm">确定</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);

  var confirmBtn = overlay.querySelector('.logout-btn-confirm');

  function toggle(show) { overlay.classList.toggle('hidden', !show); }

  /* Click outside or cancel closes dialog */
  overlay.addEventListener('click', function (e) {
    if (e.target === overlay || e.target.classList.contains('logout-btn-cancel')) toggle(false);
  });

  /* Confirm logout */
  function fail() {
    confirmBtn.classList.remove('loading');
    confirmBtn.textContent = '确定退出';
    alert('退出失败，请重试');
  }

  confirmBtn.addEventListener('click', function () {
    confirmBtn.classList.add('loading');
    confirmBtn.textContent = '正在退出...';
    fetch('/__logout')
      .then(function (r) { r.ok ? setTimeout(location.reload.bind(location), 300) : fail(); })
      .catch(fail);
  });

  /* 3. Inject sidebar entry (once) */
  var injected = false;
  function inject() {
    if (injected) return;
    var sidebar = document.querySelector('.sidebar');
    if (!sidebar) return;
    injected = true;
    var section = document.createElement('div');
    section.className = 'sidebar-section sidebar-logout-section';
    section.innerHTML = '<div class="sidebar-section-header"><h3>退出系统</h3></div>';
    section.querySelector('.sidebar-section-header').addEventListener('click', toggle.bind(null, true));
    sidebar.appendChild(section);
  }

  /* 4. Wait for sidebar to appear */
  inject();
  var observer = new MutationObserver(function () {
    if (injected) { observer.disconnect(); return; }
    inject();
  });
  observer.observe(document.body || document.documentElement, { childList: true, subtree: true });
})();
