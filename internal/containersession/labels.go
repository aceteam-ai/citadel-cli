package containersession

import (
	"sort"
	"strings"
)

// Label keys are the durable record, following the app_pod pattern. They carry
// identity and the declaration revision only, never a credential, command or path.
const (
	LabelSession     = "aceteam.session"
	LabelSchema      = "aceteam.session.schema"
	LabelRevision    = "aceteam.session.revision"
	LabelVolume      = "aceteam.session.volume"
	LabelHarness     = "aceteam.session.harness"
	LabelIncarnation = "aceteam.session.incarnation"
	LabelImage       = "aceteam.session.image"

	labelSchemaV1 = "1"
)

// Record is what label recovery returns.
type Record struct {
	Name        string
	Revision    string
	Volume      string
	Harness     string
	Incarnation string
	Image       string
}

// Labels renders the record for both the volume and the container.
func (r Record) Labels() map[string]string {
	m := map[string]string{
		LabelSession: r.Name, LabelSchema: labelSchemaV1, LabelRevision: r.Revision,
		LabelVolume: r.Volume, LabelHarness: r.Harness, LabelImage: r.Image,
	}
	if r.Incarnation != "" {
		m[LabelIncarnation] = r.Incarnation
	}
	return m
}

// RecordFromLabels recovers and validates a record. It fails closed: a record
// whose volume label does not equal the name-derived volume, or whose schema is
// unknown, is not a record this node may act on.
func RecordFromLabels(labels map[string]string) (Record, bool) {
	name := labels[LabelSession]
	if name == "" || labels[LabelSchema] != labelSchemaV1 {
		return Record{}, false
	}
	r := Record{
		Name: name, Revision: labels[LabelRevision], Volume: labels[LabelVolume],
		Harness: labels[LabelHarness], Incarnation: labels[LabelIncarnation], Image: labels[LabelImage],
	}
	if r.Volume != ContainerPrefix+name+VolumeSuffix || !strings.HasPrefix(r.Revision, "sha256:") {
		return Record{}, false
	}
	if r.Incarnation != "" && len(r.Incarnation) != 32 {
		return Record{}, false
	}
	return r, true
}

func labelArgs(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "--label", k+"="+labels[k])
	}
	return out
}
