//! Wire contracts for independently released Ormos relay clients and servers.
mod contract;
pub mod protocol;
#[cfg(feature = "seal")]
pub mod seal;
pub use contract::*;
pub use protocol::*;
pub const STREAM_FENCE_VERSION: &str = "4";
pub const STREAM_FENCE_VERSION_LEGACY_V0: &str = "";
pub const STREAM_FENCE_VERSION_V1: &str = "1";
pub const STREAM_FENCE_VERSION_V2: &str = "2";
pub const STREAM_FENCE_VERSION_V3: &str = "3";
pub const STREAM_FENCE_VERSION_V4: &str = "4";
pub const STREAM_FENCE_VERSION_HEADER: &str = "X-Ormos-Stream-Fence-Version";
pub const PUBLIC_KEY_HEADER: &str = "X-Ormos-Public-Key";
pub const KIND_TERMINAL: &str = "terminal";
pub const KIND_PROXY: &str = "proxy";
pub const KIND_LIST_PORTS: &str = "listports";
pub const KIND_SHUTDOWN: &str = "shutdown";
pub const KIND_EVENT: &str = "event";
pub const TERMINAL_STATE_RUNNING: &str = "running";
pub const TERMINAL_STATE_CLOSING: &str = "closing";
pub const TERMINAL_STATE_EXITED: &str = "exited";
pub const DEVICE_STATUS_PENDING: &str = "pending";
pub const DEVICE_STATUS_EXPIRED: &str = "expired";
pub const DEVICE_STATUS_APPROVED: &str = "approved";
pub const ACTION_ACK_SUCCESS: &str = "success";
pub const ACTION_ACK_REFUSED: &str = "refused";
pub const ACTION_ACK_EXPIRED: &str = "expired";

#[cfg(feature = "transport")]
pub mod transport;
