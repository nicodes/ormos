use axum::{
    Json,
    http::StatusCode,
    response::{IntoResponse, Response},
};
use serde_json::json;
pub type Result<T> = std::result::Result<T, Error>;
#[derive(Debug)]
pub struct Error {
    pub status: StatusCode,
    pub message: String,
}
impl IntoResponse for Error {
    fn into_response(self) -> Response {
        (
            self.status,
            [("cache-control", "no-store")],
            Json(json!({"error":self.message})),
        )
            .into_response()
    }
}
pub fn fail(status: u16, message: &str) -> Error {
    Error {
        status: StatusCode::from_u16(status).unwrap(),
        message: message.into(),
    }
}
pub fn invalid(message: &str) -> Error {
    fail(400, message)
}
impl From<std::io::Error> for Error {
    fn from(_: std::io::Error) -> Self {
        fail(503, "local operation unavailable")
    }
}
