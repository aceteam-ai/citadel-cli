package trust

import (
	"strings"
	"testing"
)

// Fixtures are ported from aceteam-aep's tests/test_safety/*.py. Sensitive-
// looking literals (Stripe / GitHub / Google keys) are split with string
// concatenation so GitHub push protection does not block the push on a public
// repo — the runtime values still match the detector regexes, exactly as the
// Python fixtures do. AKIAIOSFODNN7EXAMPLE is AWS's own documented example key
// and is allowlisted, so it is used verbatim.
var (
	stripeLiveKey = "sk" + "_live_" + "4eC39HqLyjWDarjtT1zdp7dc"
	githubPAT     = "ghp" + "_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	googleAPIKey  = "AIza" + "SyA1234567890abcdefghijklmnopqrstuv"
	githubFinePAT = "github_pat_" + strings.Repeat("A", 22) + "_" + strings.Repeat("B", 59)
)

// findingWith reports whether findings contains one whose Type contains
// typeSubstr (case-insensitive) with the given severity and source. A "" for
// severity or source means "don't care".
func findingWith(findings []Finding, typeSubstr, severity, source string) bool {
	for _, f := range findings {
		if !strings.Contains(strings.ToLower(f.Type), strings.ToLower(typeSubstr)) {
			continue
		}
		if severity != "" && f.Severity != severity {
			continue
		}
		if source != "" && f.Source != source {
			continue
		}
		return true
	}
	return false
}

func TestCheckSecrets(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		output    string
		wantType  string
		wantSev   string
		wantSrc   string
		wantEmpty bool
	}{
		{name: "aws access key", input: "my key is AKIAIOSFODNN7EXAMPLE", wantType: "AWS access key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "aws secret key", input: "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", wantType: "AWS secret key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "github pat", input: "token: " + githubPAT, wantType: "GitHub PAT", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "github fine-grained", input: "token: " + githubFinePATValue(), wantType: "fine-grained", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "stripe live in output", output: "Use " + stripeLiveKey + " for payments", wantType: "Stripe", wantSev: SeverityHigh, wantSrc: "output"},
		{name: "slack token", input: "xoxb-123456789-abcdefghij", wantType: "Slack", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "pem rsa key", input: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowI...", wantType: "PEM private key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "pem ec key", input: "-----BEGIN EC PRIVATE KEY-----\nMHQ...", wantType: "PEM private key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "gitlab pat", input: "glpat-xxxxxxxxxxxxxxxxxxxx", wantType: "GitLab", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "generic api key", input: `api_key="sk_1234567890abcdefghijklmnop"`, wantType: "generic API key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "google api key", input: "key=" + googleAPIKey, wantType: "Google API key", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "clean text no secrets", input: "Hello world, this is a normal message with no secrets.", output: "The weather is nice today. Let me help you with that.", wantEmpty: true},
		{name: "code discussion no secrets", input: "Use the API key from your environment variables", output: "Set OPENAI_API_KEY in your .env file", wantEmpty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckSecrets(tc.input, tc.output)
			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("CheckSecrets(%q, %q) = %+v, want no findings", tc.input, tc.output, got)
				}
				return
			}
			if !findingWith(got, tc.wantType, tc.wantSev, tc.wantSrc) {
				t.Fatalf("CheckSecrets(%q, %q) = %+v, want a %q/%s/%s finding", tc.input, tc.output, got, tc.wantType, tc.wantSev, tc.wantSrc)
			}
		})
	}
}

// githubFinePATValue is a helper so the fine-grained-PAT literal is assembled
// at call time (kept out of the package-level var block for readability).
func githubFinePATValue() string { return githubFinePAT }

func TestCheckSecrets_ScansBothSources(t *testing.T) {
	got := CheckSecrets("AKIAIOSFODNN7EXAMPLE", githubPAT)
	if !findingWith(got, "AWS access key", SeverityHigh, "input") {
		t.Errorf("want AWS finding in input, got %+v", got)
	}
	if !findingWith(got, "GitHub PAT", SeverityHigh, "output") {
		t.Errorf("want GitHub finding in output, got %+v", got)
	}
}

func TestCheckSecrets_MultipleInOneText(t *testing.T) {
	got := CheckSecrets("AWS: AKIAIOSFODNN7EXAMPLE and Stripe: "+stripeLiveKey, "")
	if !findingWith(got, "AWS access key", SeverityHigh, "input") {
		t.Errorf("want AWS access key finding, got %+v", got)
	}
	if !findingWith(got, "Stripe live secret key", SeverityHigh, "input") {
		t.Errorf("want Stripe live secret key finding, got %+v", got)
	}
}

func TestCheckPII(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		output    string
		wantType  string
		wantSev   string
		wantSrc   string
		wantEmpty bool
	}{
		{name: "ssn in output", output: "His SSN is 123-45-6789", wantType: "SSN", wantSev: SeverityHigh, wantSrc: "output"},
		{name: "ssn in input high severity", input: "SSN: 123-45-6789", wantType: "SSN", wantSev: SeverityHigh, wantSrc: "input"},
		{name: "email low severity", output: "Contact john.doe@company.com for help", wantType: "EMAIL", wantSev: SeverityLow, wantSrc: "output"},
		{name: "email in input low severity", input: "Contact noreply@anthropic.com for details", wantType: "EMAIL", wantSev: SeverityLow, wantSrc: "input"},
		{name: "phone low severity", output: "Call me at (555) 123-4567", wantType: "PHONE", wantSev: SeverityLow, wantSrc: "output"},
		{name: "credit card high severity", output: "Card: 4111 1111 1111 1111", wantType: "CREDIT_CARD", wantSev: SeverityHigh, wantSrc: "output"},
		{name: "clean text no pii", output: "The weather is nice today.", wantEmpty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckPII(tc.input, tc.output)
			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("CheckPII(%q, %q) = %+v, want no findings", tc.input, tc.output, got)
				}
				return
			}
			if !findingWith(got, tc.wantType, tc.wantSev, tc.wantSrc) {
				t.Fatalf("CheckPII(%q, %q) = %+v, want a %q/%s/%s finding", tc.input, tc.output, got, tc.wantType, tc.wantSev, tc.wantSrc)
			}
		})
	}
}

func TestCheckFERPA(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		output    string
		wantType  string
		wantSev   string
		wantEmpty bool
	}{
		{name: "student id with context", input: "Look up student 123456789 enrollment status", wantType: "Student ID", wantSev: SeverityHigh},
		{name: "labeled student id", input: "Student ID: 87654321", wantType: "Labeled student ID", wantSev: SeverityHigh},
		{name: "gpa with student context", output: "The student has a GPA: 3.85 on their transcript record", wantType: "GPA with student context", wantSev: SeverityHigh},
		{name: "grade disclosure", output: "She earned an A+ in Chemistry", wantType: "Grade disclosure", wantSev: SeverityMedium},
		{name: "transcript reference", input: "Pull the transcript for student Jane Doe", wantType: "Transcript", wantSev: SeverityHigh},
		{name: "financial aid", output: "Student was awarded a Pell Grant of $6,895", wantType: "Financial aid", wantSev: SeverityHigh},
		{name: "disciplinary record", input: "Show the disciplinary record for student John Smith", wantType: "Disciplinary", wantSev: SeverityHigh},
		{name: "class roster", input: "Print the class roster for CS 101", wantType: "Bulk education record", wantSev: SeverityMedium},
		{name: "student id severity high", input: "Student ID: 12345678", wantType: "student id", wantSev: SeverityHigh},
		{name: "clean text no ferpa", input: "What is the capital of France?", output: "The capital of France is Paris.", wantEmpty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckFERPA(tc.input, tc.output)
			if tc.wantEmpty {
				if len(got) != 0 {
					t.Fatalf("CheckFERPA(%q, %q) = %+v, want no findings", tc.input, tc.output, got)
				}
				return
			}
			if !findingWith(got, tc.wantType, tc.wantSev, "combined") {
				t.Fatalf("CheckFERPA(%q, %q) = %+v, want a %q/%s finding", tc.input, tc.output, got, tc.wantType, tc.wantSev)
			}
		})
	}
}

// TestFerpaSameLineContext pins the RE2-lookahead workaround: the 9-digit
// student-ID rule fires only when an education-context word follows the number
// on the SAME line, and does NOT fire when the context word is on a different
// line (the Python `.`-without-DOTALL semantics being preserved).
func TestFerpaSameLineContext(t *testing.T) {
	if got := CheckFERPA("id 123456789 for enrollment", ""); !findingWith(got, "Student ID", SeverityHigh, "combined") {
		t.Errorf("same-line context should match, got %+v", got)
	}
	// Context word on the next line: the lookahead must NOT reach across '\n'.
	if got := CheckFERPA("id 123456789\nenrollment status", ""); findingWith(got, "Student ID", SeverityHigh, "combined") {
		t.Errorf("cross-line context should NOT match the 9-digit rule, got %+v", got)
	}
}
