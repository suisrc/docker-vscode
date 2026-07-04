(function () {
  if (window.__kinLogoutBtn) return;
  window.__kinLogoutBtn = true;

  var BTN_ID = '__kin_logout_action';

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
      // Use fetch (no page navigation) so the browser stays on the current URL
      // and the Referer is preserved for the subsequent login redirect.
      fetch('/__logout', { credentials: 'include' })
        .then(function () { window.location.reload(); })
        .catch(function () { window.location.reload(); });
    });
    li.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        fetch('/__logout', { credentials: 'include' })
          .then(function () { window.location.reload(); })
          .catch(function () { window.location.reload(); });
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
    if (window.__kinLogoutObs) return;
    var mo = new MutationObserver(function () { sync(); });
    mo.observe(document.documentElement, { childList: true, subtree: true });
    window.__kinLogoutObs = mo;
  }

  if (document.documentElement) {
    start();
  } else {
    document.addEventListener('DOMContentLoaded', start);
  }
})();
