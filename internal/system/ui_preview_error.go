//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"io"
	"net/http"
)

func previewUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.WriteHeader(http.StatusBadGateway)
	io.WriteString(w, previewUnavailablePage)
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
h1 { margin: 0; font-size: 20px; font-weight: 600; }
p { margin: 0; color: #9ca3af; font-size: 14px; line-height: 1.7; }
</style>
</head>
<body>
<main role="status">
<h1>No app listening</h1>
<p>Start your app in the terminal, then refresh the preview.</p>
</main>
</body>
</html>`
