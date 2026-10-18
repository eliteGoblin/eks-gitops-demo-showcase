#!/usr/bin/env python3
"""Log in through Keycloak's real login page with Authorization Code + PKCE, without a browser.

Used by scripts/verify.sh against environments that (correctly) have no
password-grant client. It does what a browser does: open the authorization
URL, submit the login form, catch the redirect carrying the code, then
exchange code + PKCE verifier at the token endpoint.

  pkce-login.py login   ISSUER CLIENT_ID REDIRECT_URI USERNAME   (password on stdin)
  pkce-login.py refresh ISSUER CLIENT_ID REFRESH_TOKEN
  pkce-login.py refresh-status ISSUER CLIENT_ID REFRESH_TOKEN

login and refresh print the token response JSON (and fail on any HTTP error).
refresh-status prints {"http_status": N, "body": <JSON body>} for ANY HTTP answer,
so a test can tell "Keycloak refused the token" (400 invalid_grant) from "Keycloak
was never reached" (no output, exit 2). Set SSL_CERT_FILE for networks that re-sign TLS.
"""
import base64
import hashlib
import html
import http.cookiejar
import json
import os
import re
import secrets
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request

# A named client instead of urllib's default "Python-urllib/3.x": behind Cloudflare,
# the Browser Integrity Check (on by default) refuses that signature with 403
# (error code 1010) before the request reaches Keycloak.
USER_AGENT = "eks-gitops-demo-e2e/1 (pkce-login.py)"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None  # surface the 302 so we can read the code from Location


def opener():
    ctx = ssl.create_default_context(cafile=os.environ.get("SSL_CERT_FILE"))
    jar = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(
        urllib.request.HTTPSHandler(context=ctx), urllib.request.HTTPCookieProcessor(jar), NoRedirect)
    op.addheaders = [("User-Agent", USER_AGENT)]
    return op


def token_request(op, issuer, form):
    data = urllib.parse.urlencode(form).encode()
    try:
        with op.open(f"{issuer}/protocol/openid-connect/token", data) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        print(e.read().decode(), file=sys.stderr)
        sys.exit(f"token request failed: HTTP {e.code}")


def token_status(op, issuer, form):
    """The token endpoint's HTTP status and JSON body, whatever the status."""
    data = urllib.parse.urlencode(form).encode()
    try:
        with op.open(f"{issuer}/protocol/openid-connect/token", data) as r:
            return {"http_status": r.status, "body": json.load(r)}
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            body = json.loads(raw)
        except ValueError:
            body = {"raw": raw[:200]}
        return {"http_status": e.code, "body": body}
    except (urllib.error.URLError, OSError) as e:
        sys.stderr.write(f"token endpoint unreachable: {e}\n")
        sys.exit(2)


def login(issuer, client_id, redirect_uri, username, password):
    op = opener()
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    state = secrets.token_urlsafe(16)
    query = urllib.parse.urlencode({
        "response_type": "code", "client_id": client_id, "redirect_uri": redirect_uri, "scope": "openid",
        "state": state, "code_challenge": challenge, "code_challenge_method": "S256"})
    with op.open(f"{issuer}/protocol/openid-connect/auth?{query}") as r:
        page = r.read().decode()
    form = re.search(r'<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"', page)
    if not form:
        sys.exit("login form not found (wrong client or redirect URI?)")
    body = urllib.parse.urlencode({"username": username, "password": password}).encode()
    try:
        op.open(html.unescape(form.group(1)), body)
        sys.exit("no redirect after login (bad credentials?)")
    except urllib.error.HTTPError as e:
        if e.code not in (302, 303):
            sys.exit(f"login failed: HTTP {e.code}")
        params = urllib.parse.parse_qs(urllib.parse.urlparse(e.headers["Location"]).query)
    if params.get("state", [None])[0] != state or "code" not in params:
        sys.exit(f"bad redirect: {params}")
    return token_request(op, issuer, {
        "grant_type": "authorization_code", "client_id": client_id, "code": params["code"][0],
        "redirect_uri": redirect_uri, "code_verifier": verifier})


def main(argv):
    if len(argv) == 6 and argv[1] == "login":
        tokens = login(*argv[2:6], sys.stdin.readline().rstrip("\n"))
    elif len(argv) == 5 and argv[1] == "refresh":
        tokens = token_request(opener(), argv[2], {
            "grant_type": "refresh_token", "client_id": argv[3], "refresh_token": argv[4]})
    elif len(argv) == 5 and argv[1] == "refresh-status":
        tokens = token_status(opener(), argv[2], {
            "grant_type": "refresh_token", "client_id": argv[3], "refresh_token": argv[4]})
    else:
        sys.exit(__doc__)
    json.dump(tokens, sys.stdout)


if __name__ == "__main__":
    main(sys.argv)
