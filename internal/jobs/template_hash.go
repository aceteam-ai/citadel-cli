// internal/jobs/template_hash.go
//
// Anti-tamper hash for RUN_JOB_TEMPLATE (citadel-cli#1149, aceteam#10288).
//
// A node recomputes the executable-manifest hash of a template it is asked to
// run and refuses if it does not match the approved content_hash in the job
// payload, so a compromised or buggy coordinator cannot silently swap a
// different manifest (a different `runner`, say) in under a hash the node owner
// already approved.
//
// The recomputation MUST be byte-identical to the platform's authority,
// `compute_template_hash` in python-backend/utils/database/job_templates.py:
//
//	canonical = json.dumps(
//	    {"template_key", "version", "input_schema", "output_schema", "runner"},
//	    sort_keys=True, separators=(",", ":"),   # ensure_ascii defaults to True
//	)
//	sha256(canonical.encode("utf-8")).hexdigest()
//
// Go's encoding/json is NOT a drop-in for this: json.Marshal escapes <, >, &
// (Python leaves them raw) and emits non-ASCII as raw UTF-8 (Python's default
// ensure_ascii escapes it to \uXXXX). So this file re-implements Python's
// `py_encode_basestring_ascii` + sort_keys + compact separators exactly, and is
// pinned to cross-language goldens in template_hash_test.go.
package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// computeTemplateManifestHash returns the sha256 hex of the canonical JSON of
// the executable manifest {template_key, version, input_schema, output_schema,
// runner}. inputSchema/outputSchema/runner are the raw JSON the node received;
// they are decoded with UseNumber so an integer schema constraint (e.g.
// "minLength": 5) canonicalizes as "5", never "5.0".
// Exported so the internal/worker RUN_JOB_TEMPLATE handler (which already
// imports internal/jobs via the legacy adapter) can recompute the hash without
// a second copy of the canonicalizer.
func ComputeTemplateManifestHash(templateKey string, version int, inputSchema, outputSchema, runner json.RawMessage) (string, error) {
	inV, err := decodeCanonicalValue(inputSchema)
	if err != nil {
		return "", fmt.Errorf("input_schema: %w", err)
	}
	outV, err := decodeCanonicalValue(outputSchema)
	if err != nil {
		return "", fmt.Errorf("output_schema: %w", err)
	}
	runV, err := decodeCanonicalValue(runner)
	if err != nil {
		return "", fmt.Errorf("runner: %w", err)
	}
	manifest := map[string]interface{}{
		"template_key":  templateKey,
		"version":       version,
		"input_schema":  inV,
		"output_schema": outV,
		"runner":        runV,
	}
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, manifest); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// decodeCanonicalValue decodes raw JSON into a Go value using json.Number for
// all numbers, so their original textual form (integer vs float) is preserved
// through re-serialization. An empty input decodes to a JSON null.
func decodeCanonicalValue(raw json.RawMessage) (interface{}, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// writeCanonicalJSON serializes v to match Python json.dumps(sort_keys=True,
// separators=(",",":"), ensure_ascii=True). Supports the value shapes that come
// out of decodeCanonicalValue (map/slice/string/json.Number/bool/nil) plus the
// int used for the version field.
func writeCanonicalJSON(buf *bytes.Buffer, v interface{}) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, val)
	case int:
		buf.WriteString(strconv.Itoa(val))
	case json.Number:
		buf.WriteString(string(val))
	case float64:
		// Fallback only: decodeCanonicalValue uses json.Number, so schema
		// numbers never reach here. Kept so an int-typed caller value can't
		// panic the serializer.
		buf.WriteString(strconv.FormatFloat(val, 'g', -1, 64))
	case map[string]interface{}:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte order == Unicode code-point order, matching Python
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []interface{}:
		buf.WriteByte('[')
		for i, e := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("canonical json: unsupported type %T", v)
	}
	return nil
}

// writeCanonicalString re-implements Python's py_encode_basestring_ascii:
// escape " and \; use the short escapes for \b \f \n \r \t; \uXXXX for every
// other control char (<0x20); leave printable ASCII 0x20-0x7E (including <, >,
// &) literal; and \uXXXX-escape everything >0x7E, as a surrogate pair above the
// BMP.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			buf.WriteString(`\\`)
		case '"':
			buf.WriteString(`\"`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			switch {
			case r >= 0x20 && r <= 0x7E:
				buf.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(buf, `\u%04x`, r)
			default:
				r -= 0x10000
				hi := 0xd800 | ((r >> 10) & 0x3ff)
				lo := 0xdc00 | (r & 0x3ff)
				fmt.Fprintf(buf, `\u%04x\u%04x`, hi, lo)
			}
		}
	}
	buf.WriteByte('"')
}
