// Package rules compiles a deliberately bounded Sigma subset. Rules are data:
// loading a pack never runs code, reads logs, contacts a service, or reads time.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
	"github.com/aceteam-ai/citadel-cli/internal/stream/input"
	"gopkg.in/yaml.v3"
)

const maxRuleBytes = 1 << 20
const maxPackBytes = 8 << 20
const maxRules = 128

type RuleEntry struct {
	ID string `json:"id" yaml:"id"`
	Version string `json:"version" yaml:"version"`
	File string `json:"file" yaml:"file"`
	Optional bool `json:"optional,omitempty" yaml:"optional,omitempty"`
	Fixtures []string `json:"fixtures,omitempty" yaml:"fixtures,omitempty"`
}

type Manifest struct {
	ID string `json:"id" yaml:"id"`
	Version string `json:"version" yaml:"version"`
	Parser string `json:"parser" yaml:"parser"`
	FieldMap map[string]string `json:"field_map,omitempty" yaml:"field_map,omitempty"`
	Enrichments []string `json:"enrichments,omitempty" yaml:"enrichments,omitempty"`
	Rules []RuleEntry `json:"rules" yaml:"rules"`
	Digest string `json:"digest,omitempty" yaml:"digest,omitempty"`
}

type Refusal struct {
	RuleID string `json:"rule_id"`
	Reason string `json:"reason"`
}

// CompileError contains reviewed rule identifiers and bounded compiler
// diagnostics, never an input file path or a log record. CLI drivers can show
// unsupported feature names without exposing filesystem read errors.
type CompileError struct { RuleID string; Reason string }
func (e *CompileError) Error()string{return "rule "+e.RuleID+" refused: "+e.Reason}
func refused(id string,err error)error{reason:=err.Error();if len(reason)>1024{reason="compiler diagnostic exceeds limit"};return &CompileError{id,reason}}

type extension struct {
	Escalate *bool `yaml:"escalate"`
	GroupBy []string `yaml:"group_by"`
	Window string `yaml:"window"`
	Dedup string `yaml:"dedup"`
	Absence string `yaml:"absence"`
	Rate *rateSpec `yaml:"rate"`
	Owner string `yaml:"owner"`
	Cooldown string `yaml:"cooldown"`
	EpisodeKey []string `yaml:"episode_key"`
	EvidenceFields []string `yaml:"evidence_fields"`
	SeverityMap map[string]string `yaml:"severity_map"`
	FPBudget map[string]any `yaml:"fp_budget"`
}

type rateSpec struct {
	Capacity uint64 `yaml:"capacity"`
	Every string `yaml:"every"`
}

type correlation struct {
	Type string `yaml:"type"`
	Rules []string `yaml:"rules"`
	GroupBy []string `yaml:"group-by"`
	Timespan string `yaml:"timespan"`
	Condition map[string]any `yaml:"condition"`
}

type compiledRule struct {
	ID, Version, Name, SigmaID string
	Optional bool
	Test predicate
	Correlation *correlation
	Extension extension
	Span, Dedup, Absence time.Duration
	RateEvery time.Duration
	Compare string
	Threshold uint64
	ValueField string
}

type Pack struct {
	Manifest Manifest
	Digest string
	Refused []Refusal
	rules []*compiledRule
}

func (p *Pack) ParserOptions() event.ParserOptions {
	return event.ParserOptions{FieldMap:p.Manifest.FieldMap, Enrichments:p.Manifest.Enrichments}
}

// LoadPack accepts a directory containing pack.yml, or an explicit manifest.
// Rule files are opened relative to an os.Root: neither traversal nor symlinks
// can escape the selected pack. Optional compilation failures remain visible.
func LoadPack(name string) (*Pack, error) {
	info, err := os.Stat(name)
	if err != nil { return nil, err }
	if info.IsDir() { name = filepath.Join(name,"pack.yml") }
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil { return nil, err }
	defer root.Close()
	manifestBytes, err := readBounded(root,filepath.Base(name),256<<10)
	if err != nil { return nil, err }
	var m Manifest
	if err := decodeStrict(manifestBytes,&m); err != nil { return nil, fmt.Errorf("pack manifest: %w",err) }
	if !validID(m.ID) || !validID(m.Version) || len(m.Rules)==0 || len(m.Rules)>maxRules {
		return nil, fmt.Errorf("pack needs bounded id, version and 1 to %d rules",maxRules)
	}
	switch m.Parser { case "combined","openssh","syslog","syslog3164","syslog5424","json","jsonl": default: return nil,refused("manifest",fmt.Errorf("unsupported parser: %s",m.Parser)) }
	for _, name := range m.Enrichments { if name!="source.ip_subnet24" { return nil,fmt.Errorf("unsupported enrichment: %s",name) } }
	if len(m.FieldMap)>256 { return nil,fmt.Errorf("field_map exceeds 256 entries") }
	p := &Pack{Manifest:m,Refused:[]Refusal{}}
	entries := append([]RuleEntry(nil),m.Rules...)
	sort.Slice(entries,func(i,j int)bool{return entries[i].ID<entries[j].ID})
	canonical := m
	canonical.Rules = entries
	canonical.Digest = ""
	canon,err := json.Marshal(canonical)
	if err!=nil { return nil,err }
	h := sha256.New()
	writeDigestPart(h,canon)
	seenIDs,seenFiles := map[string]bool{},map[string]bool{}
	total := len(manifestBytes)
	for _, entry := range entries {
		if !validID(entry.ID) || !validID(entry.Version) || seenIDs[entry.ID] || seenFiles[entry.File] || !fs.ValidPath(entry.File) || strings.Contains(entry.File,"\\") {
			return nil,fmt.Errorf("invalid or duplicate rule entry: %q",entry.ID)
		}
		seenIDs[entry.ID],seenFiles[entry.File] = true,true
		for _, fixture := range entry.Fixtures { if !fs.ValidPath(fixture) { return nil,fmt.Errorf("invalid fixture path for %s",entry.ID) } }
		data,err := readBounded(root,entry.File,maxRuleBytes)
		if err!=nil { return nil,fmt.Errorf("rule %s: %w",entry.ID,err) }
		total += len(data)
		if total>maxPackBytes { return nil,fmt.Errorf("pack exceeds %d bytes",maxPackBytes) }
		writeDigestPart(h,[]byte(entry.File))
		writeDigestPart(h,data)
		r,err := compileRule(entry,data)
		if err!=nil {
			if !entry.Optional { return nil,refused(entry.ID,err) }
			p.Refused=append(p.Refused,Refusal{entry.ID,err.Error()})
			continue
		}
		p.rules=append(p.rules,r)
	}
	p.Digest="sha256:"+hex.EncodeToString(h.Sum(nil))
	if m.Digest!="" && m.Digest!=p.Digest { return nil,fmt.Errorf("pack digest mismatch") }
	if err:=p.orderRules();err!=nil{return nil,refused("correlation",err)}
	return p,nil
}

func validID(s string) bool {
	if len(s)==0 || len(s)>128 { return false }
	for _,c:=range s { if !(c>='a'&&c<='z'||c>='A'&&c<='Z'||c>='0'&&c<='9'||strings.ContainsRune("._-",c)){return false} }
	return true
}

func readBounded(root *os.Root,name string,limit int)([]byte,error){
	f,err:=input.OpenRegular(root,name)
	if err!=nil{return nil,err}
	defer f.Close()
	info,err:=f.Stat()
	if err!=nil{return nil,err}
	if !info.Mode().IsRegular(){return nil,fmt.Errorf("pack input is not a regular file")}
	b,err:=io.ReadAll(io.LimitReader(f,int64(limit)+1))
	if err!=nil{return nil,err}
	if len(b)>limit{return nil,fmt.Errorf("pack input exceeds %d bytes",limit)}
	return b,nil
}

func writeDigestPart(w io.Writer,b []byte){fmt.Fprintf(w,"%d:",len(b));_,_=w.Write(b)}

func decodeStrict(data []byte,out any)error{
	var n yaml.Node
	d:=yaml.NewDecoder(bytes.NewReader(data))
	if err:=d.Decode(&n);err!=nil{return err}
	count:=0
	var check func(*yaml.Node,int)error
	check=func(n *yaml.Node,depth int)error{
		count++
		if depth>32||count>16384{return fmt.Errorf("YAML structure exceeds limit")}
		if n.Kind==yaml.AliasNode||n.Anchor!=""{return fmt.Errorf("unsupported YAML feature: anchors or aliases")}
		if n.Kind==yaml.MappingNode{
			keys:=map[string]bool{}
			for i:=0;i<len(n.Content);i+=2{
				k:=n.Content[i]
				if k.Kind!=yaml.ScalarNode||k.Tag!="!!str"||keys[k.Value]||k.Value=="<<"{return fmt.Errorf("invalid or duplicate YAML key: %s",k.Value)}
				keys[k.Value]=true
			}
		}
		for _,c:=range n.Content{if err:=check(c,depth+1);err!=nil{return err}}
		return nil
	}
	if err:=check(&n,0);err!=nil{return err}
	var extra yaml.Node
	if err:=d.Decode(&extra);err!=io.EOF{return fmt.Errorf("unsupported YAML feature: multiple documents")}
	d=yaml.NewDecoder(bytes.NewReader(data));d.KnownFields(true)
	return d.Decode(out)
}

func compileRule(entry RuleEntry,data []byte)(*compiledRule,error){
	var raw map[string]any
	if err:=decodeStrict(data,&raw);err!=nil{return nil,err}
	allowed:=map[string]bool{}
	for _,k:=range strings.Fields("title id name status description author date modified references tags logsource detection fields falsepositives level related correlation x-aceteam"){allowed[k]=true}
	for _,k:=range sortedKeys(raw){if !allowed[k]{return nil,fmt.Errorf("unsupported rule feature: %s",k)}}
	r:=&compiledRule{ID:entry.ID,Version:entry.Version,Optional:entry.Optional}
	r.Name,_=raw["name"].(string);r.SigmaID,_=raw["id"].(string)
	for _,field:=range []string{"name","id"}{if v,ok:=raw[field];ok{s,ok:=v.(string);if !ok||!validID(s){return nil,fmt.Errorf("invalid rule %s",field)}}}
	if r.Name!=""&&r.Name!=entry.ID&&r.SigmaID!=entry.ID{return nil,fmt.Errorf("manifest id does not match rule name or id")}
	if x,ok:=raw["x-aceteam"];ok{
		b,err:=yaml.Marshal(x);if err!=nil{return nil,err}
		if err:=decodeStrict(b,&r.Extension);err!=nil{return nil,fmt.Errorf("x-aceteam: %w",err)}
	}
	if r.Extension.Window!=""&&r.Extension.Window!="sliding"&&r.Extension.Window!="tumbling"{return nil,fmt.Errorf("unsupported window: %s",r.Extension.Window)}
	if err:=validateFields(r.Extension.GroupBy);err!=nil{return nil,err}
	for _,part:=range []struct{name,text string}{{"absence",r.Extension.Absence},{"cooldown",r.Extension.Cooldown},{"dedup",r.Extension.Dedup}}{
		name,text:=part.name,part.text
		if text==""{continue};v,err:=parseSpan(text);if err!=nil{return nil,fmt.Errorf("%s: %w",name,err)}
		switch name{case "dedup":r.Dedup=v;case "absence":r.Absence=v}
	}
	if r.Extension.Rate!=nil{
		if r.Extension.Rate.Capacity==0||r.Extension.Rate.Capacity>math.MaxInt64{return nil,fmt.Errorf("rate capacity must be positive int64")}
		var err error;r.RateEvery,err=parseSpan(r.Extension.Rate.Every);if err!=nil{return nil,err}
		if r.Extension.Rate.Capacity>uint64(math.MaxInt64/int64(r.RateEvery)){return nil,fmt.Errorf("rate capacity and refill interval exceed state bounds")}
	}
	if c,ok:=raw["correlation"];ok{
		if _,both:=raw["detection"];both{return nil,fmt.Errorf("detection and correlation are mutually exclusive")}
		b,err:=yaml.Marshal(c);if err!=nil{return nil,err}
		var spec correlation
		if err:=decodeStrict(b,&spec);err!=nil{return nil,err}
		if len(spec.Rules)==0||len(spec.Rules)>64{return nil,fmt.Errorf("correlation needs 1 to 64 rules")}
		if err:=validateFields(spec.GroupBy);err!=nil{return nil,err}
		r.Span,err=parseSpan(spec.Timespan);if err!=nil{return nil,err}
		switch spec.Type{
		case "event_count","value_count":
			for _,k:=range sortedKeys(spec.Condition){
				v:=spec.Condition[k]
				if k=="field"{r.ValueField,_=v.(string);continue}
				if r.Compare!=""{return nil,fmt.Errorf("correlation needs exactly one threshold")}
				switch k{case "gte","gt","lte","lt","eq":default:return nil,fmt.Errorf("unsupported correlation condition: %s",k)}
				n,ok:=nonnegativeInteger(v);if !ok{return nil,fmt.Errorf("correlation threshold must be an integer")}
				r.Compare,r.Threshold=k,n
			}
			if r.Compare==""||spec.Type=="value_count"&&r.ValueField==""{return nil,fmt.Errorf("correlation requires threshold and value_count requires field")}
			if spec.Type=="event_count"&&r.ValueField!=""{return nil,fmt.Errorf("unsupported event_count condition: field")}
			if spec.Type=="value_count"{if err:=validateFields([]string{r.ValueField});err!=nil{return nil,err}}
		case "temporal","temporal_ordered":
			if spec.Type=="temporal_ordered"&&len(spec.Rules)<2{return nil,fmt.Errorf("temporal_ordered requires at least two rules")}
			if len(spec.Condition)!=0{return nil,fmt.Errorf("unsupported temporal condition")}
		default:return nil,fmt.Errorf("unsupported correlation type: %s",spec.Type)
		}
		r.Correlation=&spec
		if spec.Type=="temporal_ordered"&&r.Extension.Window=="tumbling"{return nil,fmt.Errorf("unsupported window feature: tumbling temporal_ordered")}
	}else{
		d,ok:=raw["detection"].(map[string]any);if !ok{return nil,fmt.Errorf("missing detection map")}
		var err error;r.Test,err=compileDetection(d);if err!=nil{return nil,err}
	}
	return r,nil
}

func parseSpan(s string)(time.Duration,error){
	if strings.HasSuffix(s,"d"){
		var days uint64
		for _,c:=range strings.TrimSuffix(s,"d"){if c<'0'||c>'9'||days>7{return 0,fmt.Errorf("invalid day timespan")};days=days*10+uint64(c-'0')}
		if days<1||days>7{return 0,fmt.Errorf("day timespan must be 1 to 7d")};return time.Duration(days)*24*time.Hour,nil
	}
	d,err:=time.ParseDuration(s)
	if err!=nil||d<=0||d>7*24*time.Hour{return 0,fmt.Errorf("invalid timespan %q: expected positive duration at most 7d",s)}
	return d,nil
}

func nonnegativeInteger(v any)(uint64,bool){
	switch n:=v.(type){case int:if n>=0{return uint64(n),true};case int64:if n>=0{return uint64(n),true};case uint64:return n,true;case uint:return uint64(n),true}
	return 0,false
}

func sortedKeys(m map[string]any)[]string{keys:=make([]string,0,len(m));for k:=range m{keys=append(keys,k)};sort.Strings(keys);return keys}

func validateFields(fields []string)error{
	if len(fields)>16{return fmt.Errorf("group-by exceeds 16 fields")}
	seen:=map[string]bool{}
	for _,field:=range fields{if field==""||len(field)>128||seen[field]{return fmt.Errorf("invalid or duplicate group-by field")};seen[field]=true}
	return nil
}

func (p *Pack) orderRules()error{
	for {
	index:=map[string]*compiledRule{}
	for _,r:=range p.rules{
		for _,alias:=range []string{r.ID,r.Name,r.SigmaID}{
			if alias==""{continue};if old:=index[alias];old!=nil&&old!=r{return fmt.Errorf("ambiguous rule reference: %s",alias)};index[alias]=r
		}
	}
	state:=map[*compiledRule]int{};ordered:=[]*compiledRule{}
	var visit func(*compiledRule)error
	visit=func(r *compiledRule)error{
		if state[r]==1{return fmt.Errorf("cyclic correlation: %s",r.ID)};if state[r]==2{return nil};state[r]=1
		if r.Correlation!=nil{
			seen:=map[string]bool{}
			for i,name:=range r.Correlation.Rules{
				dep:=index[name];if dep==nil{return fmt.Errorf("rule %s references unavailable rule %s",r.ID,name)}
				if seen[dep.ID]{return fmt.Errorf("duplicate correlation reference: %s",name)};seen[dep.ID]=true
				r.Correlation.Rules[i]=dep.ID
				if err:=visit(dep);err!=nil{return err}
			}
		}
		state[r]=2;ordered=append(ordered,r);return nil
	}
	var refused *compiledRule
	var reason error
	for _,r:=range p.rules{if err:=visit(r);err!=nil{if !r.Optional{return err};refused=r;reason=err;break}}
	if refused==nil{p.rules=ordered;sort.Slice(p.Refused,func(i,j int)bool{return p.Refused[i].RuleID<p.Refused[j].RuleID});return nil}
	p.Refused=append(p.Refused,Refusal{refused.ID,reason.Error()})
	active:=make([]*compiledRule,0,len(p.rules)-1)
	for _,r:=range p.rules{if r!=refused{active=append(active,r)}}
	p.rules=active
	}
}
