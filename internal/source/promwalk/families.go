package promwalk

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed families.yaml
var defaultFamiliesYAML []byte

// Family is one protocol encoding of a neighbor or session table.
// Vendors differ only in match/label keys; emit + Reconcile stay generic.
type Family struct {
	ID           string      `yaml:"id"`
	Proto        string      `yaml:"proto"`
	Evidence     string      `yaml:"evidence"`
	Kind         string      `yaml:"kind"` // neighbor | session
	Match        Match       `yaml:"match"`
	Reporter     []string    `yaml:"reporter"`
	LocalPort    []string    `yaml:"local_port"`
	Neighbor     FieldPick   `yaml:"neighbor"`
	NeighborPort FieldPick   `yaml:"neighbor_port"`
	Join         []string    `yaml:"join"`
	Session      []string    `yaml:"session"`
	RemoteAS     []string    `yaml:"remote_as"`
	LocalAS      []string    `yaml:"local_as"`
	PeerGroup    []string    `yaml:"peer_group"`
	Established  Established `yaml:"established"`
}

type Match struct {
	NameContains    []string `yaml:"name_contains"`
	NameNotContains []string `yaml:"name_not_contains"`
}

type FieldPick struct {
	Labels              []string `yaml:"labels"`
	WhenNameContains    []string `yaml:"when_name_contains"`
	WhenNameNotContains []string `yaml:"when_name_not_contains"`
	IgnoreValues        []string `yaml:"ignore_values"`
	Lower               bool     `yaml:"lower"`
}

type Established struct {
	Value         float64  `yaml:"value"`
	OrLabelEquals string   `yaml:"or_label_equals"`
	OrLabelKeys   []string `yaml:"or_label_keys"`
}

type familyFile struct {
	Families []Family `yaml:"families"`
}

var (
	famOnce  sync.Once
	famErr   error
	families []Family
)

func DefaultFamilies() []Family {
	famOnce.Do(func() {
		families, famErr = parseFamilies(defaultFamiliesYAML)
	})
	if famErr != nil {
		panic(famErr)
	}
	return families
}

// LoadFamiliesFile replaces the catalog (empty path keeps the embedded default).
func LoadFamiliesFile(path string) error {
	if strings.TrimSpace(path) == "" {
		_ = DefaultFamilies()
		return famErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("topology families: %w", err)
	}
	got, err := parseFamilies(b)
	if err != nil {
		return err
	}
	families = got
	return nil
}

// AppendFamiliesFile adds operator rows after the embedded catalog.
func AppendFamiliesFile(path string) error {
	_ = DefaultFamilies()
	if strings.TrimSpace(path) == "" {
		return famErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("topology families: %w", err)
	}
	got, err := parseFamilies(b)
	if err != nil {
		return err
	}
	families = append(append([]Family{}, DefaultFamilies()...), got...)
	return nil
}

func parseFamilies(b []byte) ([]Family, error) {
	var f familyFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("topology families: %w", err)
	}
	if len(f.Families) == 0 {
		return nil, fmt.Errorf("topology families: empty catalog")
	}
	return f.Families, nil
}

func matchFamily(name string, fams []Family) *Family {
	n := strings.ToLower(name)
	for i := range fams {
		f := &fams[i]
		if familyMatches(n, f.Match) {
			return f
		}
	}
	return nil
}

func familyMatches(nameLower string, m Match) bool {
	if len(m.NameContains) == 0 {
		return false
	}
	ok := false
	for _, p := range m.NameContains {
		if p != "" && strings.Contains(nameLower, strings.ToLower(p)) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	for _, p := range m.NameNotContains {
		if p != "" && strings.Contains(nameLower, strings.ToLower(p)) {
			return false
		}
	}
	return true
}

func pickField(s Sample, p FieldPick) string {
	n := strings.ToLower(s.Name)
	if len(p.WhenNameContains) > 0 {
		hit := false
		for _, w := range p.WhenNameContains {
			if w != "" && strings.Contains(n, strings.ToLower(w)) {
				hit = true
				break
			}
		}
		if !hit {
			return ""
		}
	}
	for _, w := range p.WhenNameNotContains {
		if w != "" && strings.Contains(n, strings.ToLower(w)) {
			return ""
		}
	}
	v := label(s.Labels, p.Labels...)
	if v == "" {
		return ""
	}
	if p.Lower {
		v = strings.ToLower(v)
	}
	low := strings.ToLower(strings.TrimSpace(v))
	for _, ign := range p.IgnoreValues {
		if low == strings.ToLower(ign) {
			return ""
		}
	}
	return v
}

func pickReporter(labels map[string]string, keys []string) string {
	if v := label(labels, keys...); v != "" {
		return strings.ToLower(v)
	}
	inst := label(labels, "instance")
	if inst != "" && !strings.Contains(inst, ":") {
		return strings.ToLower(inst)
	}
	return ""
}

func joinKey(f Family, src string, labels map[string]string) string {
	parts := make([]string, 0, len(f.Join)+1)
	if len(f.Join) == 0 {
		return src
	}
	for _, k := range f.Join {
		if strings.EqualFold(k, "reporter") {
			parts = append(parts, src)
			continue
		}
		parts = append(parts, label(labels, k))
	}
	return strings.Join(parts, "|")
}

func sessionUp(s Sample, e Established) bool {
	if e.Value != 0 && s.Value == e.Value {
		return true
	}
	want := strings.ToLower(strings.TrimSpace(e.OrLabelEquals))
	if want == "" {
		return false
	}
	keys := e.OrLabelKeys
	if len(keys) == 0 {
		keys = []string{"state", "conn_state"}
	}
	for _, k := range keys {
		if strings.ToLower(strings.TrimSpace(s.Labels[k])) == want {
			return true
		}
	}
	return false
}
