package relay

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Device-flow states are values on the HTTP control-plane wire. The literals
// here record what independently released agents and relays already exchange;
// comparing the constants only with each other would let both uses drift.
func TestDeviceStatusValuesArePinnedToTheirWireLiterals(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"DeviceStatusPending", DeviceStatusPending, "pending"},
		{"DeviceStatusExpired", DeviceStatusExpired, "expired"},
		{"DeviceStatusApproved", DeviceStatusApproved, "approved"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the deployed control-plane wire value %q", tc.name, tc.got, tc.want)
		}
	}
}

// All 31 JSON tag occurrences in contract.go are shared HTTP control-plane
// contracts between the api and system binaries, so none are excluded. This is
// the complete inventory, grouped by DTO:
//
//   - SystemInfo (5): id, name, hostname, online, ip_addr
//   - PortInfo (3): project, port, label
//   - ProjectInfo (4): id, name, root_dir, ports
//   - TerminalSessionInfo (4): id, project_id, project_name, session_id
//   - PortEntry (3): id, port, label
//   - DeviceStartRequest (2): client_id, hostname
//   - DeviceStartResponse (5): user_code, device_code, verification_url,
//     expires_in, interval
//   - DevicePollRequest (1): device_code
//   - DevicePollResponse (1): status
//   - ProvisionResponse (3): systemId, token, name
//
// Each populated value is encoded against hand-written JSON and that same
// literal is decoded independently. A same-struct round trip would stay green
// if a tag changed and therefore cannot guard this contract.
func TestControlPlaneDTOTagsArePinnedToLiteralPayloads(t *testing.T) {
	t.Run("SystemInfo", func(t *testing.T) {
		assertLiteralJSON(t, `{"id":"sys-1","name":"Desk","hostname":"desk.local","online":true,"ip_addr":"192.0.2.1"}`,
			SystemInfo{ID: "sys-1", Name: "Desk", Hostname: "desk.local", Online: true, IPAddr: "192.0.2.1"})
	})
	t.Run("PortInfo", func(t *testing.T) {
		assertLiteralJSON(t, `{"project":"ormos","port":8080,"label":"web"}`,
			PortInfo{Project: "ormos", Port: 8080, Label: "web"})
	})
	t.Run("ProjectInfo", func(t *testing.T) {
		assertLiteralJSON(t, `{"id":"proj-1","name":"ormos","root_dir":"/code/ormos","ports":[{"id":"port-1","port":8080,"label":"web"}]}`,
			ProjectInfo{ID: "proj-1", Name: "ormos", RootDir: "/code/ormos", Ports: []PortEntry{{ID: "port-1", Port: 8080, Label: "web"}}})
	})
	t.Run("TerminalSessionInfo", func(t *testing.T) {
		assertLiteralJSON(t, `{"id":"terminal-1","project_id":"proj-1","project_name":"ormos","session_id":"session-1"}`,
			TerminalSessionInfo{ID: "terminal-1", ProjectID: "proj-1", ProjectName: "ormos", SessionID: "session-1"})
	})
	t.Run("TerminalSessionInfo lifecycle", func(t *testing.T) {
		assertLiteralJSON(t, `{"id":"record","project_id":"proj-1","project_name":"ormos","session_id":"session-1","state":"running","generation":3}`,
			TerminalSessionInfo{ID: "record", ProjectID: "proj-1", ProjectName: "ormos", SessionID: "session-1", State: TerminalStateRunning, Generation: 3})
	})
	t.Run("PortEntry", func(t *testing.T) {
		assertLiteralJSON(t, `{"id":"port-1","port":8080,"label":"web"}`,
			PortEntry{ID: "port-1", Port: 8080, Label: "web"})
	})
	t.Run("DeviceStartRequest", func(t *testing.T) {
		assertLiteralJSON(t, `{"client_id":"client-1","hostname":"desk.local"}`,
			DeviceStartRequest{ClientID: "client-1", Hostname: "desk.local"})
	})
	t.Run("DeviceStartResponse", func(t *testing.T) {
		assertLiteralJSON(t, `{"user_code":"ABCD-EFGH","device_code":"device-1","verification_url":"https://ormos.dev/device","expires_in":600,"interval":5}`,
			DeviceStartResponse{UserCode: "ABCD-EFGH", DeviceCode: "device-1", VerificationURL: "https://ormos.dev/device", ExpiresIn: 600, Interval: 5})
	})
	t.Run("DevicePollRequest", func(t *testing.T) {
		assertLiteralJSON(t, `{"device_code":"device-1"}`,
			DevicePollRequest{DeviceCode: "device-1"})
	})
	t.Run("DevicePollResponse", func(t *testing.T) {
		assertLiteralJSON(t, `{"status":"approved","systemId":"sys-1","token":"pairing-token","name":"Desk"}`,
			DevicePollResponse{Status: "approved", ProvisionResponse: ProvisionResponse{SystemID: "sys-1", Token: "pairing-token", Name: "Desk"}})
	})
	t.Run("ProvisionResponse", func(t *testing.T) {
		assertLiteralJSON(t, `{"systemId":"sys-1","token":"pairing-token","name":"Desk"}`,
			ProvisionResponse{SystemID: "sys-1", Token: "pairing-token", Name: "Desk"})
	})
}

func assertLiteralJSON[T any](t *testing.T, wire string, want T) {
	t.Helper()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode DTO: %v", err)
	}
	if string(encoded) != wire {
		t.Errorf("DTO marshalled to\n  %s\nwant the recorded wire payload\n  %s", encoded, wire)
	}

	var got T
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("decode recorded DTO payload: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recorded DTO payload decoded to\n  %+v\nwant\n  %+v", got, want)
	}
}
