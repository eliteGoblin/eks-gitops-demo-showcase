// Package web embeds the static demo pages: the token walkthrough SPA, the
// GraphiQL playground and the shared OIDC helper.
package web

import "embed"

//go:embed index.html index.css index.js playground.html playground.css playground.js oidc.js
var FS embed.FS
