package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/eliteGoblin/eks-gitops-demo/web"
)

// directive returns one CSP directive's sources ("" when absent).
func directive(policy, name string) string {
	for _, d := range strings.Split(policy, ";") {
		f := strings.Fields(d)
		if len(f) > 0 && f[0] == name {
			return strings.Join(f[1:], " ")
		}
	}
	return ""
}

func TestPagesHaveAContentSecurityPolicy(t *testing.T) {
	h := testRouter(t, true) // issuer https://id.example/realms/demo
	tests := []struct {
		path    string
		scripts string // script-src
		styles  string // style-src must contain
	}{
		{"/", "'self'", "'self'"},
		{"/playground", "'self' https://unpkg.com", "https://unpkg.com"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(h, http.MethodGet, tc.path)
			csp := rec.Header().Get("Content-Security-Policy")
			if rec.Code != 200 || csp == "" {
				t.Fatalf("status %d, CSP %q", rec.Code, csp)
			}
			if got := directive(csp, "script-src"); got != tc.scripts {
				t.Errorf("script-src = %q, want %q", got, tc.scripts)
			}
			if !strings.Contains(directive(csp, "style-src"), tc.styles) {
				t.Errorf("style-src = %q, want it to contain %q", directive(csp, "style-src"), tc.styles)
			}
			if strings.Contains(csp, "unsafe-") {
				t.Errorf("no directive may allow inline code or eval: %q", csp)
			}
			for name, want := range map[string]string{
				"default-src":     "'self'",
				"connect-src":     "'self' https://id.example", // the identity provider's origin, from the issuer
				"frame-ancestors": "'none'",
				"base-uri":        "'none'",
				"object-src":      "'none'",
				"form-action":     "'none'", // the pages have no forms; logins navigate, they do not post
			} {
				if got := directive(csp, name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if rec.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" {
				t.Error("pages must isolate their browsing context")
			}
		})
	}
}

func TestAPIResponsesGetTheLockedDownDefaultPolicy(t *testing.T) {
	csp := get(testRouter(t, false), http.MethodGet, "/config.json").Header().Get("Content-Security-Policy")
	if directive(csp, "default-src") != "'none'" || directive(csp, "frame-ancestors") != "'none'" {
		t.Fatalf("non-page responses must carry default-src 'none': %q", csp)
	}
}

// The policies forbid inline code, so the pages must not contain any: every
// script and style comes from a file, and no element carries an on* handler.
func TestPagesHaveNoInlineScriptsOrStyles(t *testing.T) {
	scriptTag := regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)
	for _, page := range []string{"index.html", "playground.html"} {
		body, err := web.FS.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		html := string(body)
		for _, bad := range []string{"<style", " style=", " onclick=", " onload=", "javascript:"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s contains inline %q", page, bad)
			}
		}
		for _, m := range scriptTag.FindAllStringSubmatch(html, -1) {
			if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
				t.Errorf("%s contains an inline <script>: %.60q", page, m[0])
			}
		}
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	dev := testRouter(t, true)
	for path, want := range map[string]string{
		"/index.css":      "text/css",
		"/index.js":       "javascript",
		"/playground.css": "text/css",
		"/playground.js":  "javascript",
	} {
		rec := get(dev, http.MethodGet, path)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), want) {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := get(testRouter(t, false), http.MethodGet, "/playground.js"); rec.Code != 404 {
		t.Errorf("playground assets are served only with the playground: %d", rec.Code)
	}
}
