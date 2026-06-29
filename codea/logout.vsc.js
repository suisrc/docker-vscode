(function () {
  if (window.__codeaLogoutBtn) return;
  window.__codeaLogoutBtn = true;

  var BTN_ID = '__codea_logout_action';

  function createItem() {
    var li = document.createElement('li');
    li.id = BTN_ID;
    li.className = 'action-item icon';
    li.setAttribute('role', 'button');
    li.setAttribute('aria-label', '退出');
    li.setAttribute('tabindex', '0');
    li.style.cursor = 'pointer';

    var a = document.createElement('a');
    a.className = 'action-label codicon codicon-sign-out';
    a.setAttribute('aria-label', '退出');

    var ind = document.createElement('div');
    ind.className = 'active-item-indicator';

    li.appendChild(a);
    li.appendChild(ind);

    li.addEventListener('click', function () {
      window.location.href = '/__logout';
    });
    li.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        window.location.href = '/__logout';
      }
    });
    return li;
  }

  function sync() {
    var tb = document.querySelector('.activitybar ul.actions-container[role="toolbar"]');
    if (!tb) return;
    if (tb.querySelector('#' + BTN_ID)) return;
    // Insert as the first item of the toolbar, matching native action-item style.
    if (tb.firstChild) {
      tb.insertBefore(createItem(), tb.firstChild);
    } else {
      tb.appendChild(createItem());
    }
  }

  function start() {
    sync();
    if (window.__codeaLogoutObs) return;
    var mo = new MutationObserver(function () { sync(); });
    mo.observe(document.documentElement, { childList: true, subtree: true });
    window.__codeaLogoutObs = mo;
  }

  if (document.documentElement) {
    start();
  } else {
    document.addEventListener('DOMContentLoaded', start);
  }
})();
