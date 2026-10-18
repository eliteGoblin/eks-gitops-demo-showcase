(() => {
  const $ = (id) => document.getElementById(id);
  const DEFAULT_QUERY = `# The Authorization header is added for you after login.
{
  me {
    subject
    roles
    participant {
      id
      givenName
      dateOfBirth
      plans { id status budgets { category allocated spent remaining } }
    }
  }
}
`;
  let root = null;
  const oidc = window.createOIDC({ redirectUri: window.location.origin + '/playground', onChange: render, log: (m) => console.info('[oidc]', m) });

  async function fetcher(params, opts) {
    const token = oidc.accessToken();
    const headers = { 'Content-Type': 'application/json', ...((opts && opts.headers) || {}) };
    if (token) headers.Authorization = 'Bearer ' + token;
    const res = await fetch('/graphql', { method: 'POST', headers, body: JSON.stringify(params) });
    if (res.status === 401) render();
    return res.json();
  }

  function render() {
    const claims = oidc.claims();
    const loggedIn = !!claims;
    $('btnLogin').hidden = loggedIn;
    $('btnLogout').hidden = !loggedIn;
    $('btnShow').hidden = !loggedIn;
    if (!loggedIn) {
      $('who').textContent = 'not logged in';
      $('who').className = 'bad';
      $('tokenPanel').hidden = true;
      return;
    }
    const roles = ((claims.realm_access && claims.realm_access.roles) || []).filter((r) => !r.startsWith('default-') && r !== 'offline_access' && r !== 'uma_authorization');
    $('who').textContent = claims.preferred_username + ' · roles: ' + (roles.join(', ') || 'none');
    $('who').className = 'ok';
    $('token').textContent = oidc.accessToken() + '\n\n' + JSON.stringify(claims, null, 2);
    if (!root) {
      root = ReactDOM.createRoot($('graphiql'));
      root.render(React.createElement(GraphiQL, { fetcher, defaultQuery: DEFAULT_QUERY, defaultEditorToolsVisibility: false }));
    }
  }

  setInterval(() => {
    const claims = oidc.claims();
    $('exp').textContent = claims ? 'token expires in ' + Math.max(0, claims.exp - Math.floor(Date.now() / 1000)) + 's' : '';
  }, 1000);

  $('btnLogin').onclick = () => oidc.login().catch((e) => { $('who').textContent = 'login failed: ' + e.message; });
  $('btnLogout').onclick = () => oidc.logout();
  $('btnShow').onclick = () => { $('tokenPanel').hidden = !$('tokenPanel').hidden; $('btnShow').textContent = $('tokenPanel').hidden ? 'Show token' : 'Hide token'; };
  $('btnCopy').onclick = async () => { await navigator.clipboard.writeText(oidc.accessToken() || ''); $('copied').textContent = 'copied'; setTimeout(() => { $('copied').textContent = ''; }, 1500); };

  oidc.handleCallback().then(render).catch((e) => { $('who').textContent = e.message; });
})();
