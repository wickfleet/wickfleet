'use strict';
const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
const rd = params.get('rd');
const back = rd && rd.startsWith('/') && !rd.startsWith('//') && !rd.startsWith('/\\') ? rd : '/';
let setup = false;

async function post(path, body) {
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Wickfleet-Request': '1' },
    body: JSON.stringify(body),
  });
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

async function start() {
  const me = await fetch('/hub/auth/me', { credentials: 'same-origin' });
  if (me.ok) {
    if (!params.has('signout')) return location.replace(back);
    $('who').textContent = (await me.json()).user;
    $('back').href = back;
    $('signed-in').hidden = false;
    return;
  }
  const st = await fetch('/hub/auth/state').then((r) => r.json()).catch(() => ({}));
  setup = !!st.setup;
  $('token-row').hidden = !setup;
  $('token').required = setup;
  $('intro').textContent = setup ? 'No users yet. Create the first admin with the token from wickfleet-hub setup-token.' : '';
  $('submit').textContent = setup ? 'Create admin' : 'Sign in';
  $('password').autocomplete = setup ? 'new-password' : 'current-password';
  $('form').hidden = false;
}

$('form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('error').textContent = '';
  const body = { user: $('user').value.trim(), password: $('password').value };
  try {
    if (setup) await post('/hub/auth/setup', { token: $('token').value.trim(), ...body });
    else await post('/hub/auth/login', body);
    location.replace(back);
  } catch (err) {
    $('error').textContent = err.message;
  }
});

$('signout').addEventListener('click', async () => {
  try {
    await post('/hub/auth/logout', {});
    location.replace('/hub/login');
  } catch (err) {
    $('error').textContent = err.message;
  }
});

start().catch((err) => { $('error').textContent = err.message; });
