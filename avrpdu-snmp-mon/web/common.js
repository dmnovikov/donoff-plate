// Common to all the pages: the theme (dark by default, ☀/☾ switches it, kept in this browser) and,
// behind the public site (nginx with logins), the user, "выйти", and no edit controls for "view".
(function () {
  const root = document.documentElement;
  const theme = () => root.dataset.theme || 'dark';
  const bar = document.createElement('span');
  bar.className = 'ubar';
  bar.innerHTML = '<button type="button" class="ubtn" id="themebtn"></button><span id="uname"></span>';
  const head = document.querySelector('header');
  const tabs = head && head.querySelector('.tabs');
  if (head) head.insertBefore(bar, tabs || null);
  // the napi sign at the left of the header (inline: its text takes the theme color) and "created by napilab" below
  if (head) {
    const logo = document.createElement('a');
    logo.className = 'napi'; logo.href = '/'; logo.title = 'napi · все устройства';
    head.insertBefore(logo, head.firstChild);
    fetch('/napi.svg').then(r => r.ok ? r.text() : '').then(t => { logo.innerHTML = t; }).catch(() => {});
  }
  const foot = document.querySelector('footer') || document.body.appendChild(document.createElement('footer'));
  const by = document.createElement('div');
  by.className = 'by'; by.textContent = 'created by napilab';
  foot.appendChild(by);
  const tb = bar.querySelector('#themebtn');
  const showTheme = () => { tb.textContent = theme() === 'dark' ? '☀' : '☾'; tb.title = theme() === 'dark' ? 'светлая тема' : 'тёмная тема'; };
  tb.addEventListener('click', () => {
    root.dataset.theme = theme() === 'dark' ? 'light' : 'dark';
    try { localStorage.setItem('pdu.theme', root.dataset.theme); } catch (e) {}
    showTheme();
  });
  showTheme();
  fetch('/api/me', { cache: 'no-store' }).then(r => r.json()).then(me => {
    root.dataset.role = me.role || 'admin';
    if (!me.user) return;
    const u = bar.querySelector('#uname');
    u.innerHTML = '<span class="uwho"></span><button type="button" class="ubtn" id="logout">выйти</button>';
    u.querySelector('.uwho').textContent = me.user;
    u.querySelector('.uwho').title = me.role === 'view' ? 'только просмотр' : 'полный доступ';
    // basic auth has no logout: a request with a wrong login replaces the one the browser keeps
    u.querySelector('#logout').addEventListener('click', () => {
      const x = new XMLHttpRequest();
      x.open('GET', '/api/me?logout=' + Date.now(), true, 'logout', 'logout');
      x.onloadend = () => { location.href = '/'; };
      x.send();
    });
  }).catch(() => {});
})();
