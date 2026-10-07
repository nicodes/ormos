//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nicodes/ormos/internal/ui"
)

type uiDiscardResponse struct {
	header http.Header
	size   int
}

func (w *uiDiscardResponse) Header() http.Header { return w.header }
func (w *uiDiscardResponse) WriteHeader(int)     {}
func (w *uiDiscardResponse) Write(data []byte) (int, error) {
	w.size += len(data)
	return len(data), nil
}

func BenchmarkUIServeEmbeddedTerminalBundle(b *testing.B) {
	static, err := ui.Dist()
	if err != nil {
		b.Fatal(err)
	}
	largest, size := "", int64(0)
	err = fs.WalkDir(static, "assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !strings.HasSuffix(name, ".js") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > size {
			largest, size = name, info.Size()
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	server := &uiServer{static: static}
	request := httptest.NewRequest(http.MethodGet, "/"+largest, nil)
	writer := &uiDiscardResponse{header: make(http.Header)}
	server.spa(writer, request)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writer.size = 0
		server.spa(writer, request)
	}
}

func TestUIStaticEncodingCachingAndMissingAssets(t *testing.T) {
	script := []byte(strings.Repeat("const value = 'compressible terminal code';\n", 1000))
	server := &uiServer{static: fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("<html>workspace</html>")},
		"assets/terminal-abc.js": &fstest.MapFile{Data: script},
	}}
	fetch := func(method, name, encoding, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, name, nil)
		r.Header.Set("Accept-Encoding", encoding)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		server.spa(w, r)
		return w
	}
	plain := fetch("GET", "/assets/terminal-abc.js", "gzip;q=0, *;q=1", "")
	if plain.Code != 200 || !bytes.Equal(plain.Body.Bytes(), script) || plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("explicit gzip refusal must override wildcard")
	}
	zipped := fetch("GET", "/assets/terminal-abc.js", "br, gzip", "")
	if zipped.Code != 200 || zipped.Header().Get("Content-Encoding") != "gzip" || zipped.Body.Len() >= len(script) {
		t.Fatal("asset was not compressed")
	}
	reader, err := gzip.NewReader(bytes.NewReader(zipped.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(decoded, script) {
		t.Fatal("compressed representation changed source bytes")
	}
	if zipped.Header().Get("Vary") != "Accept-Encoding" || zipped.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("cache validators must distinguish representations")
	}
	for _, representation := range []struct{ encoding, etag string }{{"", plain.Header().Get("ETag")}, {"gzip", zipped.Header().Get("ETag")}} {
		cached := fetch("GET", "/assets/terminal-abc.js", representation.encoding, representation.etag)
		if cached.Code != 304 || cached.Body.Len() != 0 {
			t.Fatalf("revalidation: status=%d bytes=%d", cached.Code, cached.Body.Len())
		}
	}
	wrong := fetch("GET", "/assets/terminal-abc.js", "gzip", plain.Header().Get("ETag"))
	if wrong.Code != 200 {
		t.Fatal("identity validator incorrectly matched compressed bytes")
	}
	head := fetch("HEAD", "/assets/terminal-abc.js", "gzip", "")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("HEAD must preserve representation without a body")
	}
	r := httptest.NewRequest("GET", "/assets/terminal-abc.js", nil)
	r.Header.Set("Range", "bytes=0-7")
	rangeResponse := httptest.NewRecorder()
	server.spa(rangeResponse, r)
	if rangeResponse.Code != 206 || !bytes.Equal(rangeResponse.Body.Bytes(), script[:8]) {
		t.Fatal("identity byte range failed")
	}
	for _, missing := range []string{"/assets/missing.js", "/manifest-missing.webmanifest", "/missing.css"} {
		response := fetch("GET", missing, "gzip", "")
		if response.Code != 404 || strings.Contains(response.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("missing asset %s was cached as HTML", missing)
		}
	}
	index := fetch("GET", "/", "", "")
	route := fetch("GET", "/client-route", "", index.Header().Get("ETag"))
	if index.Code != 200 || index.Header().Get("Cache-Control") != "no-cache" || route.Code != 304 {
		t.Fatal("SPA entry must retain conditional revalidation")
	}
}

func TestUIGzipNegotiation(t *testing.T) {
	for _, row := range []struct {
		header string
		want   bool
	}{
		{"", false}, {"br", false}, {"gzip", true}, {"GZip; q=0.5", true},
		{"gzip;q=0", false}, {"*", true}, {"*;q=0", false},
		{"*;q=1, gzip;q=0", false}, {"gzip;q=0, *;q=1", false},
		{"gzip;q=invalid", false}, {"gzip;q=2", false}, {"gzip;q=-1", false},
		{"gzip;q=0.2, identity;q=1", false}, {"gzip;q=1, identity;q=0.2", true},
	} {
		encoding, _ := uiAssetEncoding(row.header, true)
		if got := encoding == "gzip"; got != row.want {
			t.Errorf("%q = %v, want %v", row.header, got, row.want)
		}
	}
}

func TestUIStaticRejectsExcludedEncodings(t *testing.T) {
	for _, encoding := range []string{"identity;q=0, gzip;q=0", "*;q=0", "br, identity;q=0"} {
		server := &uiServer{static: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>small identity-only asset</html>")}}}
		request := httptest.NewRequest("GET", "/", nil)
		request.Header.Set("Accept-Encoding", encoding)
		response := httptest.NewRecorder()
		server.spa(response, request)
		if response.Code != 406 || response.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%q: status=%d cache=%q", encoding, response.Code, response.Header().Get("Cache-Control"))
		}
	}
}
