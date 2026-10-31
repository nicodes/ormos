use axum::{
    body::Body,
    http::{HeaderMap, Method, StatusCode},
    response::{IntoResponse, Response},
};
use flate2::{Compression, write::GzEncoder};
use sha2::{Digest, Sha256};
use std::{collections::BTreeMap, io::Write};
include!(concat!(env!("OUT_DIR"), "/assets.rs"));
struct Encoding {
    data: bytes::Bytes,
    etag: String,
}
struct Asset {
    raw: Encoding,
    gzip: Option<Encoding>,
    mime: &'static str,
    immutable: bool,
}
pub struct Static {
    assets: BTreeMap<&'static str, Asset>,
}
fn encoding(bytes: bytes::Bytes) -> Encoding {
    let digest = Sha256::digest(&bytes);
    Encoding {
        data: bytes,
        etag: format!(
            "\"{}\"",
            digest
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect::<String>()
        ),
    }
}
fn mime(name: &str) -> &'static str {
    match name.rsplit('.').next().unwrap_or("") {
        "html" => "text/html; charset=utf-8",
        "js" | "mjs" => "text/javascript; charset=utf-8",
        "css" => "text/css; charset=utf-8",
        "json" => "application/json",
        "webmanifest" => "application/manifest+json",
        "svg" => "image/svg+xml",
        "png" => "image/png",
        "ico" => "image/x-icon",
        "woff2" => "font/woff2",
        _ => "application/octet-stream",
    }
}
impl Default for Static {
    fn default() -> Self {
        let mut assets = BTreeMap::new();
        for &(name, data) in ASSETS {
            let mime = mime(name);
            let raw = encoding(bytes::Bytes::from_static(data));
            let gzip = if data.len() >= 1024
                && (mime.starts_with("text/") || mime.contains("json") || mime == "image/svg+xml")
            {
                let mut encoder = GzEncoder::new(Vec::new(), Compression::best());
                encoder.write_all(data).unwrap();
                let bytes = encoder.finish().unwrap();
                (bytes.len() < data.len()).then(|| encoding(bytes.into()))
            } else {
                None
            };
            assets.insert(
                name,
                Asset {
                    raw,
                    gzip,
                    mime,
                    immutable: name.starts_with("assets/"),
                },
            );
        }
        Self { assets }
    }
}
impl Static {
    pub fn response(&self, path: &str, method: &Method, headers: &HeaderMap) -> Response {
        if method != Method::GET && method != Method::HEAD {
            return (StatusCode::METHOD_NOT_ALLOWED, [("allow", "GET, HEAD")]).into_response();
        }
        let name = path.trim_start_matches('/');
        let asset = self.assets.get(name).or_else(|| {
            if !name.rsplit('/').next().unwrap_or("").contains('.') {
                self.assets.get("index.html")
            } else {
                None
            }
        });
        let Some(asset) = asset else {
            return StatusCode::NOT_FOUND.into_response();
        };
        let (gzip_q, identity_q) = quality(
            headers
                .get("accept-encoding")
                .and_then(|v| v.to_str().ok())
                .unwrap_or(""),
        );
        let compressed = asset.gzip.is_some() && gzip_q > 0.0 && gzip_q >= identity_q;
        if !compressed && identity_q <= 0.0 {
            return StatusCode::NOT_ACCEPTABLE.into_response();
        }
        let encoding = if compressed {
            asset.gzip.as_ref().unwrap()
        } else {
            &asset.raw
        };
        let mut response = Response::new(Body::empty());
        let h = response.headers_mut();
        h.insert("content-type", asset.mime.parse().unwrap());
        h.insert("vary", "Accept-Encoding".parse().unwrap());
        h.insert("etag", encoding.etag.parse().unwrap());
        h.insert(
            "cache-control",
            if asset.immutable {
                "public, max-age=31536000, immutable"
            } else {
                "no-cache"
            }
            .parse()
            .unwrap(),
        );
        h.insert("accept-ranges", "bytes".parse().unwrap());
        if compressed {
            h.insert("content-encoding", "gzip".parse().unwrap());
        }
        if headers
            .get("if-none-match")
            .and_then(|v| v.to_str().ok())
            .is_some_and(|v| {
                v.split(',')
                    .any(|v| v.trim() == "*" || v.trim().trim_start_matches("W/") == encoding.etag)
            })
        {
            *response.status_mut() = StatusCode::NOT_MODIFIED;
            return response;
        }
        let length = encoding.data.len();
        let mut range = 0..length;
        if let Some(value) = headers.get("range").and_then(|v| v.to_str().ok())
            && headers
                .get("if-range")
                .is_none_or(|v| v.as_bytes() == encoding.etag.as_bytes())
        {
            match byte_range(value, length) {
                Some(selected) => {
                    range = selected;
                    *response.status_mut() = StatusCode::PARTIAL_CONTENT;
                    response.headers_mut().insert(
                        "content-range",
                        format!("bytes {}-{}/{}", range.start, range.end - 1, length)
                            .parse()
                            .unwrap(),
                    );
                }
                None => {
                    *response.status_mut() = StatusCode::RANGE_NOT_SATISFIABLE;
                    response.headers_mut().insert(
                        "content-range",
                        format!("bytes */{length}").parse().unwrap(),
                    );
                    return response;
                }
            }
        }
        response
            .headers_mut()
            .insert("content-length", range.len().to_string().parse().unwrap());
        if method != Method::HEAD {
            *response.body_mut() = Body::from(encoding.data.slice(range));
        }
        response
    }
}
fn quality(value: &str) -> (f32, f32) {
    let mut gzip = None;
    let mut identity = None;
    let mut star = None;
    for item in value.split(',') {
        let mut pieces = item.trim().split(';');
        let token = pieces.next().unwrap_or("").trim().to_ascii_lowercase();
        let mut q = 1.0;
        for parameter in pieces {
            if let Some(v) = parameter.trim().strip_prefix("q=") {
                q = v
                    .parse::<f32>()
                    .ok()
                    .filter(|v| v.is_finite() && *v >= 0.0 && *v <= 1.0)
                    .unwrap_or(0.0);
            }
        }
        match token.as_str() {
            "gzip" => gzip = Some(q),
            "identity" => identity = Some(q),
            "*" => star = Some(q),
            _ => {}
        }
    }
    (
        gzip.or(star).unwrap_or(0.0),
        identity.unwrap_or(if star == Some(0.0) { 0.0 } else { 1.0 }),
    )
}
fn byte_range(value: &str, length: usize) -> Option<std::ops::Range<usize>> {
    let (start, end) = value.strip_prefix("bytes=")?.split_once('-')?;
    if length == 0 || end.contains(',') {
        return None;
    }
    if start.is_empty() {
        let count = end.parse::<usize>().ok()?.min(length);
        if count == 0 {
            return None;
        }
        Some(length - count..length)
    } else {
        let start = start.parse::<usize>().ok()?;
        let end = if end.is_empty() {
            length
        } else {
            end.parse::<usize>().ok()?.saturating_add(1).min(length)
        };
        if start >= end { None } else { Some(start..end) }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn compressed_identity_and_ranges_are_distinct() {
        let files = Static::default();
        let path = format!(
            "/{}",
            ASSETS
                .iter()
                .find(|(name, bytes)| name.ends_with(".js") && bytes.len() > 1024)
                .unwrap()
                .0
        );
        let mut headers = HeaderMap::new();
        headers.insert("accept-encoding", "gzip;q=0, identity;q=0".parse().unwrap());
        assert_eq!(files.response("/", &Method::GET, &headers).status(), 406);
        headers.insert("accept-encoding", "gzip".parse().unwrap());
        let gzip = files.response(&path, &Method::HEAD, &headers);
        assert_eq!(gzip.headers().get("content-encoding").unwrap(), "gzip");
        let plain = files.response(&path, &Method::HEAD, &HeaderMap::new());
        assert_ne!(plain.headers()["etag"], gzip.headers()["etag"]);
        assert_eq!(
            files
                .response("/missing.js", &Method::GET, &headers)
                .status(),
            404
        );
        assert_eq!(byte_range("bytes=-4", 10), Some(6..10));
        assert_eq!(byte_range("bytes=100-", 10), None);
    }
}
