package api

import "github.com/w3ft-net/weft/alerting"

// SetAlertKinds installs the alert destination kinds /etc/notify.yaml may name
// when it is saved through this server. Until it is called, only the core's.
// The api only checks entries; the coordinator sends them.
func (s *Server) SetAlertKinds(r *alerting.Registry) {
	s.alertKinds = r
}
