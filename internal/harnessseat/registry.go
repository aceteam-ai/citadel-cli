package harnessseat

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Implementation budgets, not subscription capacity or routing limits.
const MaxRegistryBytes = 1 << 20

var (
	ErrRegistry   = errors.New("seat_registry_invalid")
	metadataToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$`)
	ceilingToken  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type AuthState string

const (
	AuthUnknown   AuthState = "unknown"
	AuthSignedIn  AuthState = "signed_in"
	AuthSignedOut AuthState = "not_signed_in"
)

// Seat is a local declaration, not proof of login, entitlement or permission.
// All fields are excluded from accidental JSON serialization. Status adapters
// must independently establish observed metadata; launch admission must prove
// filesystem ownership, roots, data policy and native isolation separately.
type Seat struct {
	Identity    `yaml:",inline"`
	Label       string   `json:"-" yaml:"label"`
	Entitlement string   `json:"-" yaml:"entitlement"`
	PlanType    string   `json:"-" yaml:"plan_type,omitempty"`
	Models      []string `json:"-" yaml:"models,omitempty"`
	Roots       []string `json:"-" yaml:"roots"`
	// DataCeiling is an opaque policy selector, NOT an invented ordered lattice.
	// This codec never decides whether any nonpublic work is permitted.
	DataCeiling   string    `json:"-" yaml:"data_ceiling"`
	AccountDigest string    `json:"-" yaml:"account_digest,omitempty"`
	Enabled       bool      `json:"-" yaml:"enabled"`
	Auth          AuthState `json:"-" yaml:"auth"`
}

type PublicSeat struct {
	PublicIdentity
	Label       string    `json:"label"`
	Entitlement string    `json:"entitlement"`
	PlanType    string    `json:"plan_type,omitempty"`
	Models      []string  `json:"models"`
	DataCeiling string    `json:"data_ceiling"`
	Enabled     bool      `json:"enabled"`
	Auth        AuthState `json:"auth"`
}

func (s Seat) PublicProjection(salt []byte, nodeIdentity string) (PublicSeat, error) {
	if err := validateSeat(s); err != nil {
		return PublicSeat{}, err
	}
	identity, err := s.Identity.Projection(salt, nodeIdentity)
	if err != nil {
		return PublicSeat{}, err
	}
	return PublicSeat{PublicIdentity: identity, Label: s.Label, Entitlement: s.Entitlement, PlanType: s.PlanType, Models: append([]string{}, s.Models...), DataCeiling: s.DataCeiling, Enabled: s.Enabled, Auth: s.Auth}, nil
}

func validateSeat(s Seat) error {
	if s.Identity.Validate() != nil || !cleanText(s.Label, 80) || !ceilingToken.MatchString(s.DataCeiling) || len(s.Models) > 64 || len(s.Roots) > 32 {
		return ErrRegistry
	}
	switch s.Entitlement {
	case "consumer", "workspace", "api_key", "local":
	default:
		return ErrRegistry
	}
	switch s.Auth {
	case AuthUnknown, AuthSignedOut:
	case AuthSignedIn:
		if s.AccountDigest == "" {
			return ErrRegistry
		}
	default:
		return ErrRegistry
	}
	if s.Harness == Muse && s.DataCeiling != "public" {
		return ErrRegistry
	}
	if s.PlanType != "" && !metadataToken.MatchString(s.PlanType) {
		return ErrRegistry
	}
	models, roots := map[string]bool{}, map[string]bool{}
	for _, model := range s.Models {
		if !metadataToken.MatchString(model) || models[model] {
			return ErrRegistry
		}
		models[model] = true
	}
	for _, root := range s.Roots {
		if !privateRoot(root) || roots[root] {
			return ErrRegistry
		}
		roots[root] = true
	}
	if s.AccountDigest != "" {
		raw, err := hex.DecodeString(s.AccountDigest)
		if err != nil || len(raw) != 32 || strings.ToLower(s.AccountDigest) != s.AccountDigest {
			return ErrRegistry
		}
	}
	return nil
}

// RegistryDocument owns only the seats block. Other root sections are retained
// as opaque YAML, not interpreted as validated profiles or launch permission.
// The injected resolver must be the trusted local owner resolver, never remote
// metadata. Encoding re-resolves owners rather than trusting a supplied HOME.
type RegistryDocument struct {
	document *yaml.Node
	seats    []Seat
	resolve  func(string) (string, error)
}

func cloneSeats(seats []Seat) []Seat {
	copy := append([]Seat{}, seats...)
	for i := range copy {
		copy[i].Roots = append([]string{}, seats[i].Roots...)
		copy[i].Models = append([]string{}, seats[i].Models...)
	}
	return copy
}

func (d RegistryDocument) Seats() []Seat { return cloneSeats(d.seats) }

func (d RegistryDocument) WithSeats(seats []Seat) RegistryDocument {
	d.seats = cloneSeats(seats)
	return d
}

func DecodeRegistry(raw []byte, resolve func(string) (string, error)) (RegistryDocument, error) {
	if len(raw) == 0 || len(raw) > MaxRegistryBytes {
		return RegistryDocument{}, ErrRegistry
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if decoder.Decode(&doc) != nil || len(doc.Content) != 1 || decoder.Decode(new(yaml.Node)) != io.EOF || !plainYAML(&doc) {
		return RegistryDocument{}, ErrRegistry
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return RegistryDocument{}, ErrRegistry
	}
	var seats []Seat
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "seats" {
			continue
		}
		list := root.Content[i+1]
		if list.Kind != yaml.SequenceNode || len(list.Content) > 64 {
			return RegistryDocument{}, ErrRegistry
		}
		for _, entry := range list.Content {
			if !seatNode(entry) {
				return RegistryDocument{}, ErrRegistry
			}
			seat := Seat{Auth: AuthUnknown}
			if entry.Decode(&seat) != nil {
				return RegistryDocument{}, ErrRegistry
			}
			seats = append(seats, seat)
		}
	}
	if err := resolveAndValidateSeats(seats, resolve); err != nil {
		return RegistryDocument{}, err
	}
	return RegistryDocument{document: &doc, seats: cloneSeats(seats), resolve: resolve}, nil
}

func resolveAndValidateSeats(seats []Seat, resolve func(string) (string, error)) error {
	if len(seats) > 64 || (len(seats) != 0 && resolve == nil) {
		return ErrRegistry
	}
	ids := map[string]bool{}
	var bindings []AccountBinding
	for i := range seats {
		home, err := resolve(seats[i].Owner)
		if err != nil {
			return ErrRegistry
		}
		seats[i].HomeDir = home
		if validateSeat(seats[i]) != nil || ids[seats[i].ID] {
			return ErrRegistry
		}
		ids[seats[i].ID] = true
		if seats[i].AccountDigest != "" {
			bindings = append(bindings, AccountBinding{seats[i].ID, seats[i].Harness, seats[i].AccountDigest})
		}
		for _, other := range seats[:i] {
			if pathContains(other.ConfigDir, seats[i].ConfigDir) || pathContains(seats[i].ConfigDir, other.ConfigDir) {
				return ErrRegistry
			}
		}
	}
	return ValidateBindings(bindings)
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (d RegistryDocument) Encode() ([]byte, error) {
	if d.document == nil {
		return nil, ErrRegistry
	}
	seats := cloneSeats(d.seats)
	if err := resolveAndValidateSeats(seats, d.resolve); err != nil {
		return nil, err
	}
	clone := cloneYAML(d.document)
	root := clone.Content[0]
	var list yaml.Node
	if list.Encode(seats) != nil {
		return nil, ErrRegistry
	}
	found := false
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "seats" {
			root.Content[i+1], found = &list, true
			break
		}
	}
	if !found {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "seats"}, &list)
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if encoder.Encode(clone) != nil || encoder.Close() != nil || output.Len() > MaxRegistryBytes {
		return nil, ErrRegistry
	}
	return output.Bytes(), nil
}

func cloneYAML(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = cloneYAML(child)
	}
	return &copy
}

func plainYAML(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Alias != nil {
		return false
	}
	switch node.Kind {
	case yaml.DocumentNode:
	case yaml.MappingNode:
		if node.Tag != "!!map" {
			return false
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return false
		}
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null", "!!timestamp":
		default:
			return false
		}
	default:
		return false
	}
	if node.Kind == yaml.MappingNode {
		if len(node.Content)%2 != 0 {
			return false
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if !plainYAML(child) {
			return false
		}
	}
	return true
}

func seatNode(node *yaml.Node) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	types := map[string]string{"id": "!!str", "harness": "!!str", "os_user": "!!str", "config_dir": "!!str", "label": "!!str", "entitlement": "!!str", "plan_type": "!!str", "data_ceiling": "!!str", "account_digest": "!!str", "auth": "!!str", "enabled": "!!bool", "models": "!!seq", "roots": "!!seq"}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		tag, known := types[key]
		if !known || value.Tag != tag {
			return false
		}
		if tag == "!!seq" {
			if value.Kind != yaml.SequenceNode {
				return false
			}
			for _, entry := range value.Content {
				if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
					return false
				}
			}
		} else if value.Kind != yaml.ScalarNode {
			return false
		}
		seen[key] = true
	}
	for _, required := range []string{"id", "harness", "os_user", "config_dir", "label", "entitlement", "roots", "data_ceiling", "enabled"} {
		if !seen[required] {
			return false
		}
	}
	return true
}
