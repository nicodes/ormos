//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

type uiAsset struct {
	data, gzip                  []byte
	etag, gzipETag, contentType string
}

// Immutable embedded files are prepared once per server, rather than copied
// and compressed on every reload. The cache is bounded by the embedded build.
func loadUIAssets(static fs.FS) (map[string]uiAsset, error) {
	assets := make(map[string]uiAsset)
	compressor, err := gzip.NewWriterLevel(nil, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(static, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(static, name)
		if err != nil {
			return err
		}
		asset := uiAsset{data: data, etag: uiAssetETag(data), contentType: mime.TypeByExtension(path.Ext(name))}
		switch path.Ext(name) {
		case ".js":
			asset.contentType = "text/javascript; charset=utf-8"
		case ".webmanifest":
			asset.contentType = "application/manifest+json; charset=utf-8"
		}
		switch path.Ext(name) {
		case ".js", ".css", ".html", ".svg", ".webmanifest":
			if len(data) >= 1024 {
				var compressed bytes.Buffer
				compressor.Reset(&compressed)
				if _, err := compressor.Write(data); err != nil {
					return err
				}
				if err := compressor.Close(); err != nil {
					return err
				}
				if compressed.Len() < len(data) {
					asset.gzip = compressed.Bytes()
					asset.gzipETag = uiAssetETag(asset.gzip)
				}
			}
		}
		assets[name] = asset
		return nil
	})
	return assets, err
}

func uiAssetETag(data []byte) string { return fmt.Sprintf(`"%x"`, sha256.Sum256(data)) }

func serveUIAsset(w http.ResponseWriter, r *http.Request, name string, asset uiAsset) {
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	data, etag := asset.data, asset.etag
	w.Header().Set("Vary", "Accept-Encoding")
	encoding, acceptable := uiAssetEncoding(r.Header.Get("Accept-Encoding"), len(asset.gzip) > 0)
	if !acceptable {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "No acceptable asset encoding", http.StatusNotAcceptable)
		return
	}
	if encoding == "gzip" {
		data, etag = asset.gzip, asset.gzipETag
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("ETag", etag)
	if asset.contentType != "" {
		w.Header().Set("Content-Type", asset.contentType)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// Respect explicit gzip/identity weights and wildcard refusals. Missing or
// empty negotiation uses identity; supported compression is preferred unless
// the client explicitly gives identity a higher weight.
func uiAssetEncoding(value string, compressed bool) (string, bool) {
	gzipQ, identityQ, wildcardQ := 0.0, 1.0, 0.0
	gzipListed, identityListed, wildcardListed := false, false, false
	for _, item := range strings.Split(value, ",") {
		fields := strings.Split(item, ";")
		encoding := strings.ToLower(strings.TrimSpace(fields[0]))
		if encoding != "gzip" && encoding != "identity" && encoding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range fields[1:] {
			key, raw, found := strings.Cut(parameter, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
		}
		switch encoding {
		case "gzip":
			gzipQ, gzipListed = quality, true
		case "identity":
			identityQ, identityListed = quality, true
		case "*":
			wildcardQ, wildcardListed = quality, true
		}
	}
	if wildcardListed {
		if !gzipListed {
			gzipQ = wildcardQ
		}
		if !identityListed && wildcardQ == 0 {
			identityQ = 0
		}
	}
	if compressed && gzipQ > 0 && (!identityListed || gzipQ >= identityQ) {
		return "gzip", true
	}
	if identityQ > 0 {
		return "", true
	}
	return "", false
}
