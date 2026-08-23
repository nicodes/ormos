//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"encoding/json"
	"io"
)

// handleListPorts writes the listening ports the relay may know about, as a
// JSON array, and returns. The listing is itself a disclosure — it enumerates
// the services this machine runs — so each port faces the same proxyAllowed
// decision a dial would, the request is audited, and a policy that cannot be
// read discloses nothing at all.
func (d *system) handleListPorts(stream io.Writer) {
	pol, policyOK := d.livePolicy()
	if !policyOK {
		d.audit.record(auditEntry{Event: "list-ports", Detail: "policy unreadable", Allowed: false})
		if err := json.NewEncoder(stream).Encode([]int{}); err != nil {
			d.logf("list ports encode: %v", err)
		}
		return
	}
	ports, err := listeningPorts()
	if err != nil {
		d.audit.record(auditEntry{Event: "list-ports", Detail: "discovery failed", Allowed: false})
		d.logf("list ports discovery: %v", err)
		if err := json.NewEncoder(stream).Encode([]int{}); err != nil {
			d.logf("list ports encode: %v", err)
		}
		return
	}
	allowed := make([]int, 0, len(ports))
	for _, port := range ports {
		if ok, _ := pol.proxyAllowed(port); ok {
			allowed = append(allowed, port)
		}
	}
	d.audit.record(auditEntry{Event: "list-ports", Allowed: true})
	if err := json.NewEncoder(stream).Encode(allowed); err != nil {
		d.logf("list ports encode: %v", err)
	}
}
