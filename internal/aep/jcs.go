package aep

// This file holds the ONE canonicalizer the v3 receipt canon is built on:
// pure RFC 8785 (JSON Canonicalization Scheme) over the plain logical object.
// docs/design-canon-framework.md §7 recommends it as the long-term v3 target,
// replacing the hand-rolled newline-delimited Canonicalize/CanonicalizeV2 forms
// (which are non-injective: a '\n' inside a free-string field shifts the field
// boundaries without changing the bytes, so one signature can vouch for two
// distinct receipts -- design §2 R1.1 / §9.4). JCS is injective by construction
// (P1), transport-robust on the 1-vs-1.0 hazard that motivated the v2 'f' 6
// score rule (P3, §3.2), and extends to nested fields without a new framing
// rule (P4).
//
// CROSS-REPO LOCKSTEP: v3 is NOT the emitted default. v2 (CanonicalizeV2)
// remains the shape every node emits; this path is ready-but-not-default so the
// eventual cutover is a one-line flip once the aceteam verifier (aceteam #9287)
// and its goldens move to the same JCS canon. Until then, treat this as the
// specification-plus-conformance-corpus half of that cross-repo contract:
// testdata/v3/corpus.json pins the exact bytes a Python rfc8785 implementation
// must reproduce.

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"

	"github.com/gowebpki/jcs"
)

// jcsMaxSafeInteger is 2^53. RFC 8785 numbers are IEEE-754 doubles, so only
// integers strictly inside the I-JSON safe range (RFC 7493 §2.2:
// [-(2^53)+1, 2^53-1]) are representable exactly. The design's §9.2 empirical
// run showed the fail-closed Python library `rfc8785` raising IntegerDomainError
// on 2^53 ITSELF (not merely above it), while `gowebpki/jcs` silently rounds it
// through a double -- so accepting exactly 2^53 on the Go side would bake in the
// very cross-language divergence this canon exists to remove. The domain check
// below therefore rejects |n| >= 2^53 (i.e. anything outside [-(2^53)+1,
// 2^53-1]), matching rfc8785. Per design R1/R2, any integer that could exceed
// this (a ns timestamp, a large byte count) must be carried as a STRING, not a
// JSON number.
const jcsMaxSafeInteger = int64(1) << 53 // 9007199254740992

// CanonicalizeJCS is THE single RFC 8785 canonicalizer, shared by the v3 receipt
// (CanonicalizeV3) AND the v3 verdict object (VerdictHashV3) -- design R5, one
// canonicalizer for every signed-or-hashed JSON object in the receipt system.
//
// It marshals v to JSON and re-serializes it per RFC 8785. Two implementation
// rules the spec leaves to the caller are enforced here so a receipt can never
// be signed into a form the fail-closed Python verifier would reject:
//
//   - Non-finite floats (NaN/+Inf/-Inf) are rejected (design R3). They are not
//     representable in JSON at all; json.Marshal would also error, but walking
//     the Go value first yields a clearer message and never reaches the encoder.
//   - Integers outside the I-JSON safe range are rejected (design R1) -- see
//     jcsMaxSafeInteger. The check walks the Go VALUE (not the marshaled JSON),
//     because Go's json.Marshal renders a large float64 (e.g. 1.2e20) as a
//     bare integer-looking string with no decimal point, indistinguishable from
//     an integer literal in the marshaled text; only the Go type disambiguates
//     "float that happens to be integral" (allowed, IEEE double) from "integer
//     out of safe range" (rejected).
//
// HTML escaping applied by encoding/json ("<" for '<', etc.) is normalized
// away by jcs.Transform's parse-and-re-emit, which restores the raw character
// per RFC 8785; non-ASCII stays raw UTF-8; object keys are sorted by UTF-16
// code units. signature/public_key_fingerprint stripping (design R4) is the
// caller's responsibility (CanonicalizeV3 does it) -- this function canonicalizes
// exactly the object it is given.
func CanonicalizeJCS(v any) ([]byte, error) {
	if err := assertJCSNumberDomain(reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("aep: marshal for JCS: %w", err)
	}
	out, err := jcs.Transform(data)
	if err != nil {
		return nil, fmt.Errorf("aep: JCS transform: %w", err)
	}
	return out, nil
}

// assertJCSNumberDomain walks a Go value and enforces the two number rules
// (finite floats; integers within the I-JSON safe range) by Go type, before the
// value is marshaled. See CanonicalizeJCS's doc for why the walk is over the Go
// value rather than the marshaled JSON.
func assertJCSNumberDomain(rv reflect.Value) error {
	switch rv.Kind() {
	case reflect.Invalid:
		return nil // untyped nil (e.g. a nil map value): renders as JSON null.
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return assertJCSNumberDomain(rv.Elem())
	case reflect.Struct:
		t := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			if t.Field(i).PkgPath != "" {
				continue // unexported field: never marshaled.
			}
			if err := assertJCSNumberDomain(rv.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := rv.MapRange()
		for iter.Next() {
			if err := assertJCSNumberDomain(iter.Value()); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if err := assertJCSNumberDomain(rv.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := rv.Int()
		if n >= jcsMaxSafeInteger || n <= -jcsMaxSafeInteger {
			return fmt.Errorf("aep: integer %d is outside the JCS/I-JSON safe range [-(2^53)+1, 2^53-1]; carry it as a string", n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if n := rv.Uint(); n >= uint64(jcsMaxSafeInteger) {
			return fmt.Errorf("aep: integer %d is outside the JCS/I-JSON safe range [-(2^53)+1, 2^53-1]; carry it as a string", n)
		}
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("aep: float %v is not finite; JCS/RFC 8785 cannot represent it (carry only finite numbers)", f)
		}
	}
	return nil
}
