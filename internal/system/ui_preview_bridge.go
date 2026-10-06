//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func localPreviewPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "", fmt.Errorf("not a local path")
	}
	return u.String(), nil
}

// The bridge is an external same-origin script in the preview, so common
// script-src 'self' policies keep working. Messages only reach known control
// origins and the parent authenticates both the source window and origin.
func (s *uiServer) previewBridge() []byte {
	port := strconv.Itoa(s.controlPort)
	origins := []string{"http://127.0.0.1:" + port, "http://localhost:" + port}
	for _, authority := range s.hosts {
		authority = strings.TrimSpace(authority)
		if _, p, err := net.SplitHostPort(authority); err == nil && p == port {
			origins = append(origins, "http://"+authority, "https://"+authority)
		}
	}
	encoded, _ := json.Marshal(origins)
	return []byte(`(()=>{const allowed=` + string(encoded) + `;let origin;const report=(replace=false)=>{if(origin)parent.postMessage({type:'ormos:preview-location',path:location.pathname+location.search+location.hash,replace},origin)};for(const name of ['pushState','replaceState']){const original=history[name];history[name]=function(){const result=original.apply(this,arguments);report(name==='replaceState');return result}}addEventListener('popstate',()=>report());addEventListener('hashchange',()=>report());addEventListener('message',event=>{if(event.source!==parent||!allowed.includes(event.origin)||event.data?.type!=='ormos:preview-connect')return;origin=event.origin;report()})})();`)
}

const previewHTMLLimit = 2 << 20

var previewHeadTag = regexp.MustCompile(`(?i)<head(?:\s+(?:"[^"]*"|'[^']*'|[^'">])*)?>`)
var previewBodyTag = regexp.MustCompile(`(?i)<body(?:\s+(?:"[^"]*"|'[^']*'|[^'">])*)?>`)
var previewScriptNonce = regexp.MustCompile(`(?i)<script\b[^>]*\bnonce\s*=\s*(?:"([A-Za-z0-9+/_=-]+)"|'([A-Za-z0-9+/_=-]+)')`)

type previewBodyReader struct {
	io.Reader
	io.Closer
}

// HTML stays bounded; compressed, streaming and non-HTML bodies pass through.
// Large responses retain their original stream and response headers.
func injectPreviewBridge(res *http.Response) error {
	kind, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(kind, "text/html") || res.Body == nil || res.Header.Get("Content-Encoding") != "" || res.StatusCode != http.StatusOK || (res.Request != nil && res.Request.Method == http.MethodHead) {
		return nil
	}
	original := res.Body
	body, err := io.ReadAll(io.LimitReader(original, previewHTMLLimit+1))
	if err != nil {
		original.Close()
		return err
	}
	if len(body) > previewHTMLLimit {
		res.Body = previewBodyReader{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
		return nil
	}
	original.Close()
	nonce := ""
	if match := previewScriptNonce.FindSubmatch(body); match != nil {
		value := match[1]
		if len(value) == 0 {
			value = match[2]
		}
		nonce = ` nonce="` + string(value) + `"`
	}
	tag := []byte(`<script src="/__ormos_bridge.js"` + nonce + `></script>`)
	position := 0
	if match := previewHeadTag.FindIndex(body); match != nil {
		position = match[1]
	} else if match := previewBodyTag.FindIndex(body); match != nil {
		position = match[0]
	}
	rewritten := make([]byte, 0, len(body)+len(tag))
	rewritten = append(rewritten, body[:position]...)
	rewritten = append(rewritten, tag...)
	rewritten = append(rewritten, body[position:]...)
	res.Body = io.NopCloser(bytes.NewReader(rewritten))
	res.ContentLength = int64(len(rewritten))
	res.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	res.Header.Set("Cache-Control", "no-store")
	res.Header.Del("ETag")
	res.Header.Del("Content-MD5")
	return nil
}
