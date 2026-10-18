// Minimal OpenID Connect client (Authorization Code + PKCE S256) shared by the
// demo pages. Tokens live only in memory: a page reload forgets them on purpose.
// The access token is refreshed silently 30 s before it expires.
window.createOIDC = function createOIDC({ redirectUri, log = () => {}, onChange = () => {} }) {
  let cfg = null;
  let clientId = 'spa';
  let tokens = null; // { access, id, refresh, exp }
  let refreshTimer = null;

  const b64url = (bytes) => btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  const randomString = () => b64url(crypto.getRandomValues(new Uint8Array(32)));
  const sha256 = async (text) => b64url(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))));
  const decodePart = (s) => {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    s += '='.repeat((4 - s.length % 4) % 4);
    return JSON.parse(decodeURIComponent(Array.from(atob(s), (c) => '%' + c.charCodeAt(0).toString(16).padStart(2, '0')).join('')));
  };

  async function discover() {
    if (cfg) return cfg;
    const c = await fetch('/config.json');
    if (!c.ok) throw new Error('config failed: HTTP ' + c.status);
    const pub = await c.json();
    clientId = pub.clientId || clientId;
    const res = await fetch(pub.issuer + '/.well-known/openid-configuration');
    if (!res.ok) throw new Error('discovery failed: HTTP ' + res.status);
    cfg = await res.json();
    log('discovery ok: token endpoint ' + cfg.token_endpoint);
    return cfg;
  }

  async function login() {
    const c = await discover();
    const verifier = randomString();
    const state = randomString();
    // The verifier and state must survive the redirect; sessionStorage is cleared when the tab closes.
    sessionStorage.setItem('pkce_verifier', verifier);
    sessionStorage.setItem('oauth_state', state);
    const url = new URL(c.authorization_endpoint);
    url.search = new URLSearchParams({
      response_type: 'code', client_id: clientId, redirect_uri: redirectUri, scope: 'openid',
      state, code_challenge: await sha256(verifier), code_challenge_method: 'S256',
    });
    log('PKCE challenge created; redirecting to the identity provider');
    window.location.assign(url);
  }

  async function tokenRequest(params) {
    const c = await discover();
    const res = await fetch(c.token_endpoint, {
      method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ client_id: clientId, ...params }),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error_description || body.error || ('HTTP ' + res.status));
    tokens = { access: body.access_token, id: body.id_token || (tokens && tokens.id), refresh: body.refresh_token, exp: decodePart(body.access_token.split('.')[1]).exp };
    schedule();
    onChange();
    return body;
  }

  function schedule() {
    clearTimeout(refreshTimer);
    if (!tokens || !tokens.refresh) return;
    const ms = Math.max(tokens.exp * 1000 - Date.now() - 30000, 5000);
    refreshTimer = setTimeout(refresh, ms);
  }

  async function refresh() {
    if (!tokens || !tokens.refresh) return;
    try {
      await tokenRequest({ grant_type: 'refresh_token', refresh_token: tokens.refresh });
      log('access token refreshed silently');
    } catch (e) {
      tokens = null;
      onChange();
      log('refresh failed (' + e.message + '); log in again');
    }
  }

  // handleCallback finishes the redirect: checks state, exchanges code + verifier.
  async function handleCallback() {
    const params = new URLSearchParams(window.location.search);
    window.history.replaceState({}, '', redirectUri);
    if (params.get('error')) throw new Error('identity provider error: ' + (params.get('error_description') || params.get('error')));
    const code = params.get('code');
    if (!code) return false;
    if (params.get('state') !== sessionStorage.getItem('oauth_state')) throw new Error('state mismatch (possible CSRF); log in again');
    const verifier = sessionStorage.getItem('pkce_verifier');
    sessionStorage.removeItem('oauth_state');
    sessionStorage.removeItem('pkce_verifier');
    log('got authorization code; exchanging it with the code_verifier');
    const body = await tokenRequest({ grant_type: 'authorization_code', code, redirect_uri: redirectUri, code_verifier: verifier });
    log('token response: expires_in=' + body.expires_in + 's');
    return true;
  }

  async function logout() {
    const c = await discover();
    const url = new URL(c.end_session_endpoint);
    url.search = new URLSearchParams({ id_token_hint: (tokens && tokens.id) || '', post_logout_redirect_uri: redirectUri, client_id: clientId });
    clear();
    log('memory cleared; ending the identity provider session');
    window.location.assign(url);
  }

  function clear() {
    clearTimeout(refreshTimer);
    tokens = null;
    onChange();
  }

  return {
    login, logout, handleCallback, refresh, clear,
    accessToken: () => (tokens ? tokens.access : null),
    header: () => (tokens ? decodePart(tokens.access.split('.')[0]) : null),
    claims: () => (tokens ? decodePart(tokens.access.split('.')[1]) : null),
  };
};
