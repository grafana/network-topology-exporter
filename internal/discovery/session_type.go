package discovery

import "strings"

// InferSessionType classifies a BGP session from observed AS numbers, then
// falls back to the device's configured peer-group name.
func InferSessionType(localAS, remoteAS, peerGroup string) string {
	if localAS != "" && remoteAS != "" {
		if localAS == remoteAS {
			return SessionTypeIBGP
		}
		return SessionTypeEBGP
	}
	g := strings.ToLower(peerGroup)
	switch {
	case strings.Contains(g, "ibgp"):
		return SessionTypeIBGP
	case strings.Contains(g, "ebgp"):
		return SessionTypeEBGP
	}
	return ""
}
