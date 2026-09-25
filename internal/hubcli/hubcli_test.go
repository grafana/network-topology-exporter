package hubcli

import (
	"strings"
	"testing"

	"github.com/grafana/network-topology-exporter/internal/config"
)

func TestCheckRole(t *testing.T) {
	tests := []struct {
		name    string
		role    config.Role
		wantErr bool
	}{
		{"hub accepted", config.RoleHub, false},
		{"standalone rejected", config.RoleStandalone, true},
		{"uncoordinated rejected", config.RoleUncoordinated, true},
		{"spoke rejected", config.RoleSpoke, true},
		{"empty rejected", config.Role(""), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRole(tt.role)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckRole(%q) error = %v, wantErr %v", tt.role, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "cmd/topology-hub") {
				t.Errorf("CheckRole(%q) error %q should name cmd/topology-hub so the operator knows which binary is complaining", tt.role, err.Error())
			}
			if err != nil && !strings.Contains(err.Error(), string(tt.role)) {
				t.Errorf("CheckRole(%q) error %q should echo back the rejected role", tt.role, err.Error())
			}
		})
	}
}

func TestEffectiveListenAddr(t *testing.T) {
	tests := []struct {
		name          string
		explicitlySet bool
		flagAddr      string
		cfgAddr       string
		want          string
	}{
		{"flag explicitly set wins over config", true, ":9200", ":9100", ":9200"},
		{"flag not set falls back to config", false, ":9100", ":9200", ":9200"},
		{"flag not set, config empty stays empty", false, ":9100", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveListenAddr(tt.explicitlySet, tt.flagAddr, tt.cfgAddr)
			if got != tt.want {
				t.Errorf("EffectiveListenAddr(%v, %q, %q) = %q, want %q",
					tt.explicitlySet, tt.flagAddr, tt.cfgAddr, got, tt.want)
			}
		})
	}
}
