package relay

// This file defines the HTTP control-plane DTOs exchanged between the api
// (relay server) and the cli (system) over the relay's JSON endpoints. They are
// distinct from the tunnel wire protocol in protocol.go: that governs bytes on a
// yamux stream, these govern JSON request/response bodies. Both binaries import
// these types so the contract can't drift out of sync (previously each side
// re-declared its own copy). They live in the public relay package because they
// are the shared api↔system surface and travel with it when relay becomes its
// own module.

// SystemInfo is a system's own display info (GET /system/info).
type SystemInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Online   bool   `json:"online"`
	IPAddr   string `json:"ip_addr"`
}

// PortInfo is a single configured exposed port, labelled with its project name
// (GET /system/ports). Name-only; for the read-only ports list.
type PortInfo struct {
	Project string `json:"project"`
	Port    int    `json:"port"`
	Label   string `json:"label"`
}

// ProjectInfo is a project on a system with its exposed ports, for the system's
// own management view (GET /system/projects). Record ids are carried so the CLI
// can edit/delete specific records.
type ProjectInfo struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	RootDir string      `json:"root_dir"`
	Ports   []PortEntry `json:"ports"`
}

// TerminalSessionInfo identifies a terminal session and the project it belongs
// to. It is shared by the agent and relay control plane so their JSON field
// spelling cannot drift.
type TerminalSessionInfo struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	SessionID   string `json:"session_id"`
	State       string `json:"state,omitempty"`
	Generation  int    `json:"generation,omitempty"`
}

const (
	TerminalStateRunning = "running"
	TerminalStateClosing = "closing"
	TerminalStateExited  = "exited"
)

// PortEntry is one exposed port with its record id.
type PortEntry struct {
	ID    string `json:"id"`
	Port  int    `json:"port"`
	Label string `json:"label"`
}

// DeviceStartRequest is the body of POST /device/start, the unauthenticated
// opening of a device-authorization round: the calling system's stable client
// id and display hostname. No credentials travel here — a human approves the
// pairing out of band in the web app.
type DeviceStartRequest struct {
	ClientID string `json:"client_id"`
	Hostname string `json:"hostname"`
}

// DeviceStartResponse is returned by POST /device/start: the short code the
// user types into the web app, the opaque device code the agent polls with, the
// page to visit, and the flow's timing (durations in seconds).
type DeviceStartResponse struct {
	UserCode        string `json:"user_code"`
	DeviceCode      string `json:"device_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"` // seconds until the codes expire
	Interval        int    `json:"interval"`   // seconds the agent waits between polls
}

// DevicePollRequest is the body of POST /device/poll.
type DevicePollRequest struct {
	DeviceCode string `json:"device_code"`
}

// Device flow states reported in DevicePollResponse.Status.
const (
	// DeviceStatusPending means the code is live but not yet approved.
	DeviceStatusPending = "pending"
	// DeviceStatusExpired means the codes expired unapproved; start over.
	DeviceStatusExpired = "expired"
	// DeviceStatusApproved means the user approved the code; the pairing
	// fields are set.
	DeviceStatusApproved = "approved"
)

// DevicePollResponse is the response of POST /device/poll. The endpoint always
// answers 200 — the flow state is carried in Status, not the HTTP status. When
// Status is DeviceStatusApproved the embedded ProvisionResponse fields carry
// exactly what a successful provision did: the pairing token, system id and
// display name the agent saves to its config.
type DevicePollResponse struct {
	Status string `json:"status"`
	ProvisionResponse
}

// ProvisionResponse is the approval payload of the device flow: the system id,
// a freshly-minted pairing token (shown once, never persisted server-side), and
// the resolved display name.
type ProvisionResponse struct {
	SystemID string `json:"systemId"`
	Token    string `json:"token"`
	Name     string `json:"name"`
}
