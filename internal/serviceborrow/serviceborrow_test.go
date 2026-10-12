package serviceborrow

import (
	"errors"
	"testing"
)

func asMismatch(t *testing.T, err error) *IdentityMismatchError {
	t.Helper()
	var me *IdentityMismatchError
	if !errors.As(err, &me) {
		t.Fatalf("want *IdentityMismatchError, got %T: %v", err, err)
	}
	return me
}

func TestServiceIdentityMatch_ExactMatch(t *testing.T) {
	adv := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base", Version: "1.5.0"}
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base", Version: "1.5.0"}
	if err := adv.Match(want); err != nil {
		t.Fatalf("exact match must succeed, got: %v", err)
	}
}

func TestServiceIdentityMatch_KindCaseInsensitive(t *testing.T) {
	adv := ServiceIdentity{Kind: "Embedding", Model: "m"}
	want := ServiceIdentity{Kind: "embedding", Model: "m"}
	if err := adv.Match(want); err != nil {
		t.Fatalf("kind should match case-insensitively, got: %v", err)
	}
}

func TestServiceIdentityMatch_KindMismatch_Typed(t *testing.T) {
	adv := ServiceIdentity{Kind: "transcription", Model: "whisper"}
	want := ServiceIdentity{Kind: "embedding", Model: "whisper"}
	err := adv.Match(want)
	if err == nil {
		t.Fatal("a different service kind must be a mismatch")
	}
	if me := asMismatch(t, err); me.Field != "kind" {
		t.Fatalf("mismatch field = %q, want %q", me.Field, "kind")
	}
}

func TestServiceIdentityMatch_ModelMismatch_Typed(t *testing.T) {
	adv := ServiceIdentity{Kind: "embedding", Model: "bge-small"}
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	err := adv.Match(want)
	if err == nil {
		t.Fatal("a different model must be a mismatch")
	}
	me := asMismatch(t, err)
	if me.Field != "model" {
		t.Fatalf("mismatch field = %q, want %q", me.Field, "model")
	}
	if me.Want != "gte-multilingual-base" || me.Got != "bge-small" {
		t.Fatalf("mismatch detail want=%q got=%q, want the two model ids", me.Want, me.Got)
	}
}

func TestServiceIdentityMatch_WantedModelAgainstEmptyAdvertised_Mismatch(t *testing.T) {
	// A peer that advertises no model cannot confirm it serves the required one.
	adv := ServiceIdentity{Kind: "embedding"}
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	if err := adv.Match(want); err == nil {
		t.Fatal("a wanted model against an empty advertised model must be a mismatch")
	}
}

func TestServiceIdentityMatch_EmptyWantedModel_NoConstraint(t *testing.T) {
	adv := ServiceIdentity{Kind: "embedding", Model: "anything"}
	want := ServiceIdentity{Kind: "embedding"} // no model constraint
	if err := adv.Match(want); err != nil {
		t.Fatalf("empty wanted model should impose no constraint, got: %v", err)
	}
}

func TestServiceIdentityMatch_VersionExactCaseSensitive(t *testing.T) {
	adv := ServiceIdentity{Kind: "embedding", Version: "V1"}
	want := ServiceIdentity{Kind: "embedding", Version: "v1"}
	if err := adv.Match(want); err == nil {
		t.Fatal("version comparison is exact (case-sensitive); V1 != v1")
	}
}
