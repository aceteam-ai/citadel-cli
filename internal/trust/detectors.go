package trust

// This file ports three pure, deterministic content detectors from
// aceteam-aep (src/aceteam_aep/safety/{secrets,pii,ferpa}.py) into Go as
// local, network-free, model-free checks (aceteam #8253 slice S6). Each is a
// pure function of (input, output) that returns a list of Findings; nothing
// here loads a model, calls a network, or spawns a goroutine — the guardrail
// stays fast and fully unit-testable, exactly like CheckGrounding.
//
// Deviations from the Python reference, called out because they change
// nothing observable but are not a line-for-line port:
//   - PiiDetector's transformer NER path is dropped. The reference falls back
//     to a fixed regex set (SSN/CREDIT_CARD/EMAIL/PHONE) when transformers is
//     unavailable; this port keeps ONLY that regex set, since S6's contract is
//     "no model". The regexes are copied verbatim.
//   - Two FerpaDetector patterns use lookahead ((?=...)), which Go's RE2
//     regexp cannot compile. They are ported as a head regex plus a same-line
//     context regex (see hasSameLineContext), which preserves the reference's
//     Python `.`-without-DOTALL "context word appears later on the same line"
//     semantics.
//
// Findings are NEVER surfaced in the trust verdict — only their hash
// (evidence_hash, see verdict.go) is, per #8253's DoR §3 ("carries the
// evidence hashes, never the evidence"). A Finding's Type names WHAT was
// found (e.g. "AWS access key"), never the matched secret/PII value itself,
// so even the hashed evidence list carries no raw secret.

import "regexp"

// Severity levels a Finding can carry, ported from the reference detectors'
// per-pattern severity. severityRank orders them for CheckReport.Severity
// (the highest severity among a check's findings).
const (
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

func severityRank(s string) int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// maxSeverity returns the highest-ranked severity among findings, or "" when
// there are none (a passing check has no severity).
func maxSeverity(findings []Finding) string {
	best := ""
	for _, f := range findings {
		if severityRank(f.Severity) > severityRank(best) {
			best = f.Severity
		}
	}
	return best
}

// Finding is one detection produced by a detector. It is hashed into a
// check's evidence_hash and used to decide the check's action/severity; it is
// deliberately NOT emitted into the verdict output. Type is the human-readable
// classification (never the matched value), Source is "input"/"output" (or
// "combined" for FERPA, which searches the joined text), and Start/End are the
// byte span of the match in the scanned text (both 0 for FERPA, which reports
// one boolean finding per matched pattern rather than per occurrence).
type Finding struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

// Detector is a pure, deterministic content check. The set of detectors is
// passed to BuildVerdict explicitly (see DefaultDetectors) so a caller — and
// the mutation test — can add or drop one without editing this package, and
// so #8253 S5's policy-driven `checks: [names]` selection has a seam to plug
// into.
type Detector interface {
	// Name is the check's stable identifier as it appears in the verdict's
	// checks[] list (e.g. "secrets").
	Name() string
	// Version is the check's schema version, mirroring grounding's version: 1.
	Version() int
	// Check scans input and output and returns every Finding, deterministically.
	Check(input, output string) []Finding
}

// DefaultDetectors returns the three pure detectors registered on-node, in a
// fixed order (secrets, pii, ferpa) so the verdict's checks[] ordering — and
// therefore its verdict_hash — is stable. Grounding is NOT in this list: it
// is a scored check with a different result type (GroundingResult), assembled
// separately in BuildVerdict and always listed first.
func DefaultDetectors() []Detector {
	return []Detector{secretDetector{}, piiDetector{}, ferpaDetector{}}
}

// --- secrets (ported from safety/secrets.py) ---

// secretPattern pairs a compiled regex with the human-readable description of
// the credential it matches. Ported verbatim from _SECRET_PATTERNS; every
// match is high severity (a leaked credential is unambiguous), matching the
// reference's score=1.0/severity="high".
type secretPattern struct {
	re   *regexp.Regexp
	desc string
}

var secretPatterns = []secretPattern{
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AWS access key"},
	{regexp.MustCompile(`(?i)(?:aws_secret_access_key|AWS_SECRET)\s*[:=]\s*['"]?([0-9a-zA-Z/+=]{40})`), "AWS secret key"},
	{regexp.MustCompile(`ghp_[0-9a-zA-Z]{36}`), "GitHub PAT"},
	{regexp.MustCompile(`github_pat_[0-9a-zA-Z]{22}_[0-9a-zA-Z]{59}`), "GitHub fine-grained PAT"},
	{regexp.MustCompile(`gho_[0-9a-zA-Z]{36}`), "GitHub OAuth token"},
	{regexp.MustCompile(`ghu_[0-9a-zA-Z]{36}`), "GitHub user-to-server token"},
	{regexp.MustCompile(`ghs_[0-9a-zA-Z]{36}`), "GitHub server-to-server token"},
	{regexp.MustCompile(`glpat-[0-9a-zA-Z\-]{20,}`), "GitLab PAT"},
	{regexp.MustCompile(`sk_live_[0-9a-zA-Z]{24,}`), "Stripe live secret key"},
	{regexp.MustCompile(`pk_live_[0-9a-zA-Z]{24,}`), "Stripe live publishable key"},
	{regexp.MustCompile(`rk_live_[0-9a-zA-Z]{24,}`), "Stripe restricted key"},
	{regexp.MustCompile(`xox[bpors]-[0-9a-zA-Z\-]{10,}`), "Slack token"},
	{regexp.MustCompile(`-----BEGIN\s*(?:RSA|EC|DSA|OPENSSH|ENCRYPTED)?\s*PRIVATE KEY-----`), "PEM private key"},
	{regexp.MustCompile(`sk-[0-9a-zA-Z]{20,}T3BlbkFJ[0-9a-zA-Z]{20,}`), "OpenAI API key"},
	{regexp.MustCompile(`sk-ant-api\d{2}-[0-9a-zA-Z\-]{80,}`), "Anthropic API key"},
	{regexp.MustCompile(`AIzaSy[0-9a-zA-Z\-_]{33}`), "Google API key"},
	{regexp.MustCompile(`sq0atp-[0-9a-zA-Z\-]{22,}`), "Square access token"},
	{regexp.MustCompile(`sqOatp-[0-9a-zA-Z\-]{22,}`), "Square OAuth token"},
	{regexp.MustCompile(`EZAK[0-9a-zA-Z\-]{54,}`), "EasyPost API key"},
	{regexp.MustCompile(`(?i)(?:api[_-]?key|apikey|secret[_-]?key|access[_-]?token)\s*[:=]\s*['"]?([A-Za-z0-9\-._]{20,})`), "generic API key/token"},
}

// CheckSecrets scans input and output for leaked credentials/API keys. One
// Finding per match, per source, mirroring safety/secrets.py.
func CheckSecrets(input, output string) []Finding {
	findings := make([]Finding, 0)
	for _, src := range []struct{ name, text string }{{"input", input}, {"output", output}} {
		if src.text == "" {
			continue
		}
		for _, p := range secretPatterns {
			for _, m := range p.re.FindAllStringIndex(src.text, -1) {
				findings = append(findings, Finding{
					Type:     p.desc,
					Severity: SeverityHigh,
					Source:   src.name,
					Start:    m[0],
					End:      m[1],
				})
			}
		}
	}
	return findings
}

type secretDetector struct{}

func (secretDetector) Name() string                         { return "secrets" }
func (secretDetector) Version() int                         { return 1 }
func (secretDetector) Check(input, output string) []Finding { return CheckSecrets(input, output) }

// --- pii (regex set from safety/pii.py, model path deliberately dropped) ---

// piiPattern pairs a regex with its entity type. Ported verbatim from
// _REGEX_PATTERNS, in the reference's insertion order (SSN, CREDIT_CARD,
// EMAIL, PHONE). Note the CREDIT_CARD regex does NO Luhn check — the reference
// doesn't either, so neither does this port.
type piiPattern struct {
	entity string
	re     *regexp.Regexp
}

var piiPatterns = []piiPattern{
	{"SSN", regexp.MustCompile(`\b\d{3}[-.\s]?\d{2}[-.\s]?\d{4}\b`)},
	{"CREDIT_CARD", regexp.MustCompile(`\b(?:\d{4}[-\s]?){3}\d{4}\b`)},
	{"EMAIL", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	{"PHONE", regexp.MustCompile(`\b(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`)},
}

// piiSeverity mirrors _ENTITY_SEVERITY: identity/financial identifiers are
// high, contact info is low. Entities absent here default to medium (the
// reference's _DEFAULT_SEVERITY), though all four regex entities are listed.
var piiSeverity = map[string]string{
	"SSN":         SeverityHigh,
	"CREDIT_CARD": SeverityHigh,
	"EMAIL":       SeverityLow,
	"PHONE":       SeverityLow,
}

func piiSeverityFor(entity string) string {
	if s, ok := piiSeverity[entity]; ok {
		return s
	}
	return SeverityMedium
}

// CheckPII scans input and output for PII using the reference's regex set only
// (no NER model). One Finding per match, per source.
func CheckPII(input, output string) []Finding {
	findings := make([]Finding, 0)
	for _, src := range []struct{ name, text string }{{"input", input}, {"output", output}} {
		if src.text == "" {
			continue
		}
		for _, p := range piiPatterns {
			for _, m := range p.re.FindAllStringIndex(src.text, -1) {
				findings = append(findings, Finding{
					Type:     p.entity,
					Severity: piiSeverityFor(p.entity),
					Source:   src.name,
					Start:    m[0],
					End:      m[1],
				})
			}
		}
	}
	return findings
}

type piiDetector struct{}

func (piiDetector) Name() string                         { return "pii" }
func (piiDetector) Version() int                         { return 1 }
func (piiDetector) Check(input, output string) []Finding { return CheckPII(input, output) }

// --- ferpa (ported from safety/ferpa.py) ---

// ferpaPattern is one FERPA education-record rule. When ctx is nil, head is a
// complete RE2-compatible regex matched against the joined input+output. When
// ctx is non-nil, head is the "trigger" and ctx is a same-line lookahead (see
// hasSameLineContext) — this is how the two Python patterns that used (?=...)
// lookahead (which RE2 cannot compile) are ported.
type ferpaPattern struct {
	head     *regexp.Regexp
	ctx      *regexp.Regexp // nil for plain (non-lookahead) patterns
	desc     string
	severity string
}

var ferpaPatterns = []ferpaPattern{
	// Student ID — 9-digit with education context (was a (?=...) lookahead).
	{
		head:     regexp.MustCompile(`\b\d{9}\b`),
		ctx:      regexp.MustCompile(`(?i)(?:student|enroll|gpa|grade|transcript)`),
		desc:     "Student ID (9-digit with education context)",
		severity: SeverityHigh,
	},
	{
		head:     regexp.MustCompile(`(?i)\bstudent\s*(?:id|number|#)\s*[:\s]*\d{5,10}\b`),
		desc:     "Labeled student ID number",
		severity: SeverityHigh,
	},
	// GPA with student context (was a (?=...) lookahead).
	{
		head:     regexp.MustCompile(`(?i)\b(?:gpa|grade\s*point)\s*[:\s]*\d\.\d{1,2}\b`),
		ctx:      regexp.MustCompile(`(?i)(?:student|name|record|transcript)`),
		desc:     "GPA with student context (education record)",
		severity: SeverityHigh,
	},
	{
		head:     regexp.MustCompile(`(?i)\b(?:earned|received|got)\s+(?:an?\s+)?[A-F][+-]?\s+(?:in|for)\s+\w+`),
		desc:     "Grade disclosure (specific grade in course)",
		severity: SeverityMedium,
	},
	{
		head:     regexp.MustCompile(`(?i)\btranscript\b.*\b(?:student|name|record|ssn)\b`),
		desc:     "Transcript reference with student identifiers",
		severity: SeverityHigh,
	},
	{
		head:     regexp.MustCompile(`(?i)\b(?:enrolled|enrollment|registration)\s+(?:in|for|status)\b.*\b(?:student|name)\b`),
		desc:     "Enrollment record with student identifiers",
		severity: SeverityMedium,
	},
	{
		head:     regexp.MustCompile(`(?i)\b(?:financial\s+aid|fafsa|pell\s+grant|scholarship|loan)\b.*\b(?:student|name|amount|awarded)\b|\b(?:student|name|amount|awarded)\b.*\b(?:financial\s+aid|fafsa|pell\s+grant|scholarship|loan)\b`),
		desc:     "Financial aid record",
		severity: SeverityHigh,
	},
	{
		head:     regexp.MustCompile(`(?i)\bdisciplin\w*\b.*\b(?:student|name)\b|\b(?:student|name)\b.*\bdisciplin\w*\b|\b(?:suspension|expulsion|probation|conduct\s+violation)\b.*\b(?:student|name)\b`),
		desc:     "Disciplinary record with student identifiers",
		severity: SeverityHigh,
	},
	{
		head:     regexp.MustCompile(`(?i)\b(?:ferpa|directory\s+information)\s+(?:opt|restrict|block)`),
		desc:     "FERPA directory information restriction",
		severity: SeverityMedium,
	},
	{
		head:     regexp.MustCompile(`(?i)\b(?:class\s+roster|grade\s+book|student\s+list)\b`),
		desc:     "Bulk education record reference (class roster/grade book)",
		severity: SeverityMedium,
	},
}

// hasSameLineContext reports whether some head match is followed, later on the
// SAME line, by a ctx match. This reproduces the Python `head(?=.*ctx)`
// lookahead under `.`-without-DOTALL: `.*` cannot cross a newline, so ctx must
// appear between the head match's end and the next '\n' (or end of text).
func hasSameLineContext(text string, head, ctx *regexp.Regexp) bool {
	for _, m := range head.FindAllStringIndex(text, -1) {
		tail := text[m[1]:]
		if nl := indexByte(tail, '\n'); nl >= 0 {
			tail = tail[:nl]
		}
		if ctx.MatchString(tail) {
			return true
		}
	}
	return false
}

// indexByte is strings.IndexByte, inlined to avoid a strings import solely for
// it (this file otherwise uses only regexp).
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// CheckFERPA scans the joined input+output for FERPA-protected education
// records. Mirroring safety/ferpa.py, it searches the combined text and
// reports at most one boolean Finding per matched pattern (no per-occurrence
// spans, no source), so the Source is "combined" and Start/End are 0.
func CheckFERPA(input, output string) []Finding {
	combined := input + " " + output
	findings := make([]Finding, 0)
	for _, p := range ferpaPatterns {
		var matched bool
		if p.ctx != nil {
			matched = hasSameLineContext(combined, p.head, p.ctx)
		} else {
			matched = p.head.MatchString(combined)
		}
		if matched {
			findings = append(findings, Finding{
				Type:     p.desc,
				Severity: p.severity,
				Source:   "combined",
			})
		}
	}
	return findings
}

type ferpaDetector struct{}

func (ferpaDetector) Name() string                         { return "ferpa" }
func (ferpaDetector) Version() int                         { return 1 }
func (ferpaDetector) Check(input, output string) []Finding { return CheckFERPA(input, output) }
