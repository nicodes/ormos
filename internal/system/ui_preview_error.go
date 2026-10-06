//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

func previewUnavailable(w http.ResponseWriter, port int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, strings.ReplaceAll(previewUnavailablePage, "{{PORT}}", strconv.Itoa(port)))
}

const previewUnavailablePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>No app listening · Ormos</title>
<style>
:root { color-scheme: dark; font-family: ui-sans-serif, system-ui, sans-serif; background: #0b0f19; color: #f9fafb; }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; min-height: 100dvh; display: grid; place-items: center; padding: 24px; }
main { display: flex; flex-direction: column; align-items: center; gap: 12px; width: 100%; max-width: 340px; text-align: center; }
.icon { display: grid; place-items: center; width: 64px; height: 64px; margin-bottom: 6px; border: 1px solid #374151; border-radius: 16px; background: #111827; color: #9ca3af; }
svg { width: 30px; height: 30px; }
h1 { margin: 0; font-size: 20px; font-weight: 600; }
p { margin: 0; color: #9ca3af; font-size: 14px; line-height: 1.7; }
.port { color: #6b7280; font: 12px ui-monospace, monospace; }
</style>
</head>
<body>
<main role="status">
<div class="icon" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 9h18M6 6.5h.01M9 6.5h.01M9 13v4m6-4v4"/></svg></div>
<h1>No app listening</h1>
<p>Start your app in the terminal, then refresh the preview.</p>
<span class="port">Port {{PORT}}</span>
</main>
</body>
</html>`
