(() => {
  const $ = (id) => document.getElementById(id);
  const log = (msg) => { const d = document.createElement('div'); d.textContent = new Date().toLocaleTimeString() + '  ' + msg; $('log').prepend(d); };
  const pretty = (o) => JSON.stringify(o, null, 2);
  const oidc = window.createOIDC({ redirectUri: window.location.origin + '/', log, onChange: () => { showToken(); setStatus(oidc.accessToken() ? 'logged in' : 'not logged in', !!oidc.accessToken()); } });

  function showToken() {
    const header = oidc.header(); const payload = oidc.claims();
    $('tokHeader').textContent = header ? pretty(header) : '-';
    $('tokPayload').textContent = payload ? pretty(payload) : '-';
  }

  async function gql(query) {
    const token = oidc.accessToken();
    if (!token) { $('result').textContent = 'not logged in'; return; }
    const authHeader = 'Bearer ' + token;
    $('authHeader').textContent = 'Authorization: ' + authHeader;
    try {
      const res = await fetch('/graphql', { method: 'POST', headers: { 'Content-Type': 'application/json', 'Authorization': authHeader }, body: JSON.stringify({ query }) });
      const text = await res.text();
      let body; try { body = JSON.parse(text); } catch { body = text; }
      if (res.status === 401) { oidc.clear(); setStatus('token expired or rejected (HTTP 401): log in again', false); }
      $('result').textContent = 'HTTP ' + res.status + '\n' + (typeof body === 'string' ? body : pretty(body));
      log('POST /graphql -> HTTP ' + res.status);
    } catch (e) { $('result').textContent = 'request failed: ' + e.message; }
  }

  function setStatus(text, ok) {
    const el = $('status'); el.textContent = text; el.className = 'status ' + (ok ? 'ok' : 'bad');
    const loggedIn = !!oidc.accessToken();
    for (const id of ['btnLogout', 'btnMe', 'btnMine', 'btnRun']) $(id).disabled = !loggedIn;
    $('btnLogin').disabled = loggedIn;
  }

  const ME_QUERY = `{ me { subject roles participant { id givenName } provider { id name } } }`;

  const MINE_QUERY = `{ me { subject roles
  participant { id schemeNumber givenName familyName dateOfBirth
    plans { id status startDate endDate budgets { category allocated spent remaining }
      claims(first: 5) { totalCount edges { node { id category amount status provider { name } } } } } }
  provider { id name abn } }
  claims(first: 5) { totalCount edges { node { id amount status plan { id participant { givenName dateOfBirth } } } } } }`;

  $('btnLogin').onclick = () => oidc.login().catch((e) => setStatus('login failed: ' + e.message, false));
  $('btnLogout').onclick = () => oidc.logout().catch((e) => setStatus('logout failed: ' + e.message, false));
  $('btnMe').onclick = () => { $('query').value = ME_QUERY; gql($('query').value); };
  $('btnMine').onclick = () => { $('query').value = MINE_QUERY; gql($('query').value); };
  $('btnRun').onclick = () => gql($('query').value);

  $('query').value = ME_QUERY;
  setStatus('not logged in', false);
  oidc.handleCallback().catch((e) => setStatus(e.message, false));
})();
