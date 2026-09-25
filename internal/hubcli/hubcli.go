// Package hubcli holds cmd/topology-hub's pure CLI-wiring logic — the
// federation-role guard and the --web.listen-address flag-override rule —
// split out of main.go so it is unit-testable without spinning up a real
// HTTP/mTLS server. cmd/topology-hub's main.go stays a thin entry point:
// argv parsing, config load, and server wiring only.
//
// This is deliberately its own tiny package rather than a dependency on
// internal/app: cmd/topology-hub's main.go and debug.go/logger.go/recover.go
// already duplicate (not import) internal/app's equivalents specifically so
// this binary never links the discovery loop, credential resolver, or the
// rest of internal/app's machinery. hubcli only imports internal/config, so
// it does not reintroduce that dependency.
package hubcli

import (
	"fmt"

	"github.com/grafana/network-topology-exporter/internal/config"
)

// CheckRole returns a non-nil error when role is not config.RoleHub. This is
// the guard cmd/topology-hub applies right after config.Load: it is the
// mirror image of internal/app.Run's rejection of federation.role: hub (see
// internal/app/app.go) — cmd/topology-hub runs ONLY the hub, and every other
// role belongs in cmd/topology-exporter.
func CheckRole(role config.Role) error {
	if role != config.RoleHub {
		return fmt.Errorf(`federation.role must be "hub" for cmd/topology-hub; run cmd/topology-exporter for standalone/uncoordinated/spoke roles (got %q)`, role)
	}
	return nil
}

// EffectiveListenAddr resolves the address cmd/topology-hub's metrics server
// binds to: the --web.listen-address flag value when the operator passed it
// explicitly on argv, otherwise listen.addr from the loaded config file.
// explicitlySet mirrors flag.FlagSet.Visit's semantics — true only when the
// flag actually appeared in args, never merely because a flag.FlagSet always
// carries a default value for it.
func EffectiveListenAddr(explicitlySet bool, flagAddr, cfgAddr string) string {
	if explicitlySet {
		return flagAddr
	}
	return cfgAddr
}
