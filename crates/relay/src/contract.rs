use serde::{Deserialize, Serialize};
fn null_default<'de, D: serde::Deserializer<'de>, T: Deserialize<'de> + Default>(
    decoder: D,
) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(decoder)?.unwrap_or_default())
}
fn zero(value: &i64) -> bool {
    *value == 0
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct SystemInfo {
    #[serde(rename = "id", deserialize_with = "null_default")]
    pub id: String,
    #[serde(rename = "name", deserialize_with = "null_default")]
    pub name: String,
    #[serde(rename = "hostname", deserialize_with = "null_default")]
    pub hostname: String,
    #[serde(rename = "online", deserialize_with = "null_default")]
    pub online: bool,
    #[serde(rename = "ip_addr", deserialize_with = "null_default")]
    pub ip_addr: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct PortInfo {
    #[serde(rename = "project", deserialize_with = "null_default")]
    pub project: String,
    #[serde(rename = "port", deserialize_with = "null_default")]
    pub port: i64,
    #[serde(rename = "label", deserialize_with = "null_default")]
    pub label: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ProjectInfo {
    #[serde(rename = "id", deserialize_with = "null_default")]
    pub id: String,
    #[serde(rename = "name", deserialize_with = "null_default")]
    pub name: String,
    #[serde(rename = "root_dir", deserialize_with = "null_default")]
    pub root_dir: String,
    #[serde(rename = "ports")]
    pub ports: Option<Vec<PortEntry>>,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct TerminalSessionInfo {
    #[serde(rename = "id", deserialize_with = "null_default")]
    pub id: String,
    #[serde(rename = "project_id", deserialize_with = "null_default")]
    pub project_id: String,
    #[serde(rename = "project_name", deserialize_with = "null_default")]
    pub project_name: String,
    #[serde(rename = "session_id", deserialize_with = "null_default")]
    pub session_id: String,
    #[serde(
        rename = "state",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub state: String,
    #[serde(
        rename = "generation",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub generation: i64,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct PortEntry {
    #[serde(rename = "id", deserialize_with = "null_default")]
    pub id: String,
    #[serde(rename = "port", deserialize_with = "null_default")]
    pub port: i64,
    #[serde(rename = "label", deserialize_with = "null_default")]
    pub label: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DeviceStartRequest {
    #[serde(rename = "client_id", deserialize_with = "null_default")]
    pub client_id: String,
    #[serde(rename = "hostname", deserialize_with = "null_default")]
    pub hostname: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DeviceStartResponse {
    #[serde(rename = "user_code", deserialize_with = "null_default")]
    pub user_code: String,
    #[serde(rename = "device_code", deserialize_with = "null_default")]
    pub device_code: String,
    #[serde(rename = "verification_url", deserialize_with = "null_default")]
    pub verification_url: String,
    #[serde(rename = "expires_in", deserialize_with = "null_default")]
    pub expires_in: i64,
    #[serde(rename = "interval", deserialize_with = "null_default")]
    pub interval: i64,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DevicePollRequest {
    #[serde(rename = "device_code", deserialize_with = "null_default")]
    pub device_code: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DevicePollResponse {
    #[serde(rename = "status", deserialize_with = "null_default")]
    pub status: String,
    #[serde(flatten)]
    pub provision: ProvisionResponse,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ProvisionResponse {
    #[serde(rename = "systemId", deserialize_with = "null_default")]
    pub system_id: String,
    #[serde(rename = "token", deserialize_with = "null_default")]
    pub token: String,
    #[serde(rename = "name", deserialize_with = "null_default")]
    pub name: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct StreamHeader {
    #[serde(rename = "kind", deserialize_with = "null_default")]
    pub kind: String,
    #[serde(
        rename = "protocol_version",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub protocol_version: String,
    #[serde(
        rename = "system_id",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub system_id: String,
    #[serde(
        rename = "port",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub port: i64,
    #[serde(
        rename = "cols",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub cols: i64,
    #[serde(
        rename = "rows",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub rows: i64,
    #[serde(
        rename = "cwd",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub cwd: String,
    #[serde(
        rename = "session_id",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub session_id: String,
    #[serde(
        rename = "terminal_record_id",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub terminal_record_id: String,
    #[serde(
        rename = "terminal_generation",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub terminal_generation: i64,
    #[serde(
        rename = "action_fence",
        deserialize_with = "null_default",
        skip_serializing_if = "String::is_empty"
    )]
    pub action_fence: String,
    #[serde(
        rename = "not_after_milli",
        deserialize_with = "null_default",
        skip_serializing_if = "zero"
    )]
    pub not_after_milli: i64,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ActionAck {
    #[serde(rename = "action_fence", deserialize_with = "null_default")]
    pub action_fence: String,
    #[serde(rename = "not_after_milli", deserialize_with = "null_default")]
    pub not_after_milli: i64,
    #[serde(rename = "status", deserialize_with = "null_default")]
    pub status: String,
}
#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct Resize {
    #[serde(rename = "cols", deserialize_with = "null_default")]
    pub cols: i64,
    #[serde(rename = "rows", deserialize_with = "null_default")]
    pub rows: i64,
}
