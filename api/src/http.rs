use crate::{
    error::{Result, fail, invalid},
    preview::Previews,
    static_files::Static,
    workspace::Workspace,
};
use axum::{
    Json, Router,
    body::{Body, to_bytes},
    extract::{Request, State},
    http::{HeaderValue, Method},
    response::{IntoResponse, Response},
};
use serde::Deserialize;
use serde_json::{Value, json};
use std::{
    io::{Read, Seek, SeekFrom},
    sync::Arc,
    time::Duration,
};
use tokio::sync::OwnedSemaphorePermit;
pub struct App {
    pub workspace: Arc<Workspace>,
    pub previews: Arc<Previews>,
    files: Static,
}
impl App {
    pub fn new(workspace: Arc<Workspace>) -> Arc<Self> {
        Arc::new(Self {
            previews: Previews::new(workspace.config.clone(), workspace.stop.clone()),
            workspace,
            files: Static::default(),
        })
    }
    pub fn router(self: &Arc<Self>) -> Router {
        Router::new().fallback(handle).with_state(self.clone())
    }
    pub async fn close(&self) {
        self.workspace.shutdown().await;
        self.previews.close().await;
    }
}
async fn handle(State(app): State<Arc<App>>, request: Request) -> Response {
    let authority = request
        .headers()
        .get("host")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_owned();
    let result = if !crate::config::host(&authority, &app.workspace.config.hosts) {
        Err(fail(421, "host not allowed"))
    } else if (!matches!(*request.method(), Method::GET | Method::HEAD)
        || request
            .headers()
            .get("upgrade")
            .is_some_and(|v| v.as_bytes().eq_ignore_ascii_case(b"websocket")))
        && request
            .headers()
            .get("origin")
            .and_then(|v| v.to_str().ok())
            .is_none_or(|origin| !crate::config::origin(origin, &authority))
    {
        Err(fail(403, "origin not allowed"))
    } else {
        match app.workspace.admission() {
            Ok(permit) => {
                let response = dispatch(&app, request, &authority).await;
                response.map(|response| hold(response, permit))
            }
            Err(e) => Err(e),
        }
    };
    let mut response = result.unwrap_or_else(IntoResponse::into_response);
    let headers = response.headers_mut();
    headers.insert("x-ormos-workspace", HeaderValue::from_static("1"));
    headers.insert(
        "x-content-type-options",
        HeaderValue::from_static("nosniff"),
    );
    headers.insert("referrer-policy", HeaderValue::from_static("no-referrer"));
    let hostname = reqwest::Url::parse(&format!("http://{authority}"))
        .ok()
        .and_then(|u| u.host_str().map(str::to_owned))
        .unwrap_or_else(|| "localhost".into());
    if let Ok(value)=format!("default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' http://{hostname}:* https://{hostname}:*; frame-src 'self' http://{hostname}:* https://{hostname}:*; frame-ancestors 'none'; base-uri 'none'; form-action 'self'").parse(){headers.insert("content-security-policy",value);}
    response
}
fn data(value: Value) -> Response {
    ([("cache-control", "no-store")], Json(value)).into_response()
}
async fn dispatch(app: &Arc<App>, mut request: Request, authority: &str) -> Result<Response> {
    let path = request.uri().path().to_owned();
    let method = request.method().clone();
    let workspace = &app.workspace;
    let read = method == Method::GET || method == Method::HEAD;
    if read && path == "/api/system" {
        let policy = workspace.config.policy();
        let roots = policy
            .as_ref()
            .map(|p| p.allowed_roots.clone())
            .unwrap_or_default();
        let hostname = hostname();
        let os = if cfg!(target_os = "macos") {
            "darwin"
        } else {
            "linux"
        };
        let arch = match std::env::consts::ARCH {
            "x86_64" => "amd64",
            "aarch64" => "arm64",
            other => other,
        };
        return Ok(data(
            json!({"v":1,"version":workspace.config.version,"hostname":hostname,"os":os,"arch":arch,"shell":workspace.config.shell(),"configDir":workspace.config.directory(),"hasConfig":workspace.config.has_config(),"hasPolicy":policy.is_ok(),"allowedRoots":roots,"terminals":workspace.list().len()}),
        ));
    }
    if read && path == "/api/terminals" {
        return Ok(data(json!({"v":1,"terminals":workspace.list()})));
    }
    if read && path == "/api/ports" {
        let ports = crate::ports::listening().await?;
        let policy = workspace.config.policy();
        let rows: Vec<_> = ports
            .into_iter()
            .map(|p| json!({"port":p,"allowed":policy.as_ref().is_ok_and(|pol|pol.port(p))}))
            .collect();
        return Ok(data(
            json!({"v":1,"ports":rows,"policyError":policy.is_err()}),
        ));
    }
    if read && path == "/api/audit" {
        let directory = workspace.config.directory().to_owned();
        let entries = tokio::task::spawn_blocking(move || audit(&directory.join("sessions.log")))
            .await
            .map_err(|_| fail(503, "audit log unreadable"))??;
        return Ok(data(json!({"v":1,"entries":entries})));
    }
    if read && let Some(tail) = path.strip_prefix("/api/terminal/") {
        if let Some(id) = tail.strip_suffix("/output") {
            return Ok(data(
                json!({"v":1,"output":workspace.terminal(id)?.snapshot()}),
            ));
        }
        if let Some(id) = tail.strip_suffix("/ws")
            && method == Method::GET
        {
            return crate::websocket::upgrade(workspace.clone(), id, request).await;
        }
    }
    if path == "/api/action" || path == "/api/preview" {
        if method != Method::POST {
            return Ok((
                axum::http::StatusCode::METHOD_NOT_ALLOWED,
                [("allow", "POST")],
                Json(json!({"error":"POST only"})),
            )
                .into_response());
        }
        let body = std::mem::replace(request.body_mut(), Body::empty());
        let limit = if path == "/api/preview" { 256 } else { 4096 };
        let bytes = tokio::time::timeout(Duration::from_secs(15), to_bytes(body, limit))
            .await
            .map_err(|_| invalid("body must be one JSON object"))?
            .map_err(|_| invalid("body must be one JSON object"))?;
        if path == "/api/preview" {
            let input: Preview =
                serde_json::from_slice(&bytes).map_err(|_| invalid("Enter a valid app port."))?;
            if input.port == 0 || !matches!(input.scheme.as_str(), "http" | "https") {
                return Err(invalid("Enter a valid app port."));
            }
            let origin = request
                .headers()
                .get("origin")
                .and_then(|v| v.to_str().ok())
                .unwrap_or("");
            if !origin.starts_with(&format!("{}://", input.scheme)) {
                return Err(invalid("Use the workspace connection scheme."));
            }
            let url = reqwest::Url::parse(&format!("http://{authority}"))
                .map_err(|_| invalid("invalid workspace host"))?;
            if url.port_or_known_default() == Some(input.port)
                || input.port == workspace.config.port
            {
                return Err(fail(403, "Choose an app port, not the ormos port."));
            }
            let port = app
                .previews
                .open(
                    input.port,
                    input.scheme,
                    url.host_str().unwrap().trim_matches(['[', ']']).into(),
                )
                .await?;
            return Ok(data(json!({"port":port})));
        }
        let input: Action =
            serde_json::from_slice(&bytes).map_err(|_| invalid("body must be one JSON object"))?;
        match input.action.as_str() {
            "open" => {
                let shell = if input.shell.is_empty() {
                    workspace.config.shell()
                } else {
                    input.shell
                };
                let cwd = if input.cwd.is_empty() {
                    workspace.config.cwd.clone()
                } else {
                    input.cwd.into()
                };
                let term = workspace.open(&shell, &cwd).await?;
                return Ok(data(json!({"v":1,"id":term.info().id})));
            }
            "kill" => {
                if input.id.is_empty() {
                    return Err(invalid("kill needs an id"));
                }
                let term = workspace.terminal(&input.id)?;
                term.kill();
                return Ok(data(json!({"v":1,"id":input.id,"alive":false})));
            }
            _ => return Err(fail(403, "action not allowed")),
        }
    }
    if path.starts_with("/api/") {
        return Err(fail(404, "no such read"));
    }
    Ok(app.files.response(&path, &method, request.headers()))
}
#[derive(Deserialize, Default)]
#[serde(default, deny_unknown_fields)]
struct Action {
    action: String,
    shell: String,
    cwd: String,
    id: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Preview {
    port: u16,
    scheme: String,
}
fn hostname() -> String {
    let mut bytes = [0u8; 256];
    if unsafe { libc::gethostname(bytes.as_mut_ptr().cast(), bytes.len()) } == 0 {
        let end = bytes.iter().position(|b| *b == 0).unwrap_or(bytes.len());
        String::from_utf8_lossy(&bytes[..end]).into_owned()
    } else {
        String::new()
    }
}
fn audit(path: &std::path::Path) -> Result<Vec<Value>> {
    let Ok(meta) = std::fs::metadata(path) else {
        return Ok(vec![]);
    };
    if !meta.is_file() {
        return Err(fail(503, "audit log unreadable"));
    }
    let offset = meta.len().saturating_sub(256 << 10);
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)?;
    file.seek(SeekFrom::Start(offset))?;
    let mut bytes = Vec::new();
    file.take(256 << 10).read_to_end(&mut bytes)?;
    let mut lines = bytes.split(|b| *b == b'\n');
    if offset > 0 {
        lines.next();
    }
    let mut rows: Vec<_> = lines
        .filter_map(|line| serde_json::from_slice::<Value>(line).ok())
        .filter(Value::is_object)
        .rev()
        .take(50)
        .collect();
    rows.reverse();
    Ok(rows)
}
use std::os::unix::fs::OpenOptionsExt;
struct HeldBody {
    body: Body,
    _permit: OwnedSemaphorePermit,
}
impl http_body::Body for HeldBody {
    type Data = bytes::Bytes;
    type Error = axum::Error;
    fn poll_frame(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<std::result::Result<http_body::Frame<Self::Data>, Self::Error>>>
    {
        std::pin::Pin::new(&mut self.get_mut().body).poll_frame(cx)
    }
    fn is_end_stream(&self) -> bool {
        self.body.is_end_stream()
    }
    fn size_hint(&self) -> http_body::SizeHint {
        self.body.size_hint()
    }
}
pub fn hold(response: Response, permit: OwnedSemaphorePermit) -> Response {
    response.map(|body| {
        Body::new(HeldBody {
            body,
            _permit: permit,
        })
    })
}
