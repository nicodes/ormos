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

// PortEntry is one exposed port with its record id.
type PortEntry struct {
	ID    string `json:"id"`
	Port  int    `json:"port"`
	Label string `json:"label"`
}

// ProvisionRequest is the body of POST /system/provision: account credentials
// plus the calling system's stable client id and display info.
type ProvisionRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	ClientID string `json:"clientId"`
	Hostname string `json:"hostname"`
	Name     string `json:"name"` // optional label; server defaults to "System N"
}

// ProvisionResponse is returned on a successful provision: the system id, a
// freshly-minted pairing token (shown once, never persisted server-side), and
// the resolved display name.
type ProvisionResponse struct {
	SystemID string `json:"systemId"`
	Token    string `json:"token"`
	Name     string `json:"name"`
}
