package serviceborrow

import (
	"errors"
	"testing"
)

func TestPlanRoute_BorrowAvailable_Borrows(t *testing.T) {
	want := ServiceIdentity{Kind: "embedding", Model: "m"}
	route, err := PlanRoute(want, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route != RouteBorrow {
		t.Fatalf("route = %v, want RouteBorrow", route)
	}
}

func TestPlanRoute_NoBorrow_LocalMatches_FallsBackLocal(t *testing.T) {
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	local := &ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	route, err := PlanRoute(want, false, local)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route != RouteLocal {
		t.Fatalf("route = %v, want RouteLocal", route)
	}
}

func TestPlanRoute_NoBorrow_LocalMismatch_ServiceUnavailable(t *testing.T) {
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	local := &ServiceIdentity{Kind: "embedding", Model: "bge-small"} // wrong model
	route, err := PlanRoute(want, false, local)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable (local does not match, must not serve wrong model)", err)
	}
	if route != RouteUnavailable {
		t.Fatalf("route = %v, want RouteUnavailable", route)
	}
}

func TestPlanRoute_NoBorrow_NoLocal_ServiceUnavailable(t *testing.T) {
	want := ServiceIdentity{Kind: "embedding", Model: "gte-multilingual-base"}
	_, err := PlanRoute(want, false, nil)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable", err)
	}
}

func TestPlanRoute_NoBorrow_LocalWrongKind_ServiceUnavailable(t *testing.T) {
	want := ServiceIdentity{Kind: "embedding", Model: "m"}
	local := &ServiceIdentity{Kind: "transcription", Model: "m"}
	_, err := PlanRoute(want, false, local)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable (local is a different kind)", err)
	}
}
