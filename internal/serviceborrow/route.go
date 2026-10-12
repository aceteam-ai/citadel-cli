package serviceborrow

// Route is where a requester should run a unit of borrowable work.
type Route int

const (
	// RouteUnavailable is the zero value; PlanRoute returns it alongside
	// ErrServiceUnavailable.
	RouteUnavailable Route = iota
	// RouteBorrow: borrow the service from a verified same-owner peer over the mesh.
	RouteBorrow
	// RouteLocal: run the service on THIS node's own matching local service.
	RouteLocal
)

func (r Route) String() string {
	switch r {
	case RouteBorrow:
		return "borrow"
	case RouteLocal:
		return "local"
	default:
		return "unavailable"
	}
}

// PlanRoute decides how a requester serves a unit of work for `want`, given
// whether a verified matching peer is available to borrow from and this node's
// own local service identity (nil when the node runs no such local service).
//
// Policy (matches the issue contract verbatim — "Fallback to local ONLY when the
// local identity matches; otherwise a typed service_unavailable error"):
//
//  1. If a borrowable peer is available -> RouteBorrow. Borrowing over the mesh
//     is the primary path (#1270's whole point: a node that cannot serve
//     locally offloads to a same-owner peer).
//  2. Else fall back to local, but ONLY when the local service's identity
//     matches `want` -> RouteLocal. A non-matching local service (wrong model,
//     wrong kind) is NOT silently used.
//  3. Else -> ErrServiceUnavailable. NEVER a central or hosted fallback.
//
// borrowAvailable is the caller's conclusion after auto/explicit peer selection
// (a peer was verified same-owner AND advertised a matching service identity).
// Keeping the peer-selection I/O out of this pure function is what makes the
// fallback rule adversarially testable.
func PlanRoute(want ServiceIdentity, borrowAvailable bool, localIdentity *ServiceIdentity) (Route, error) {
	if borrowAvailable {
		return RouteBorrow, nil
	}
	if localIdentity != nil && localIdentity.Match(want) == nil {
		return RouteLocal, nil
	}
	return RouteUnavailable, ErrServiceUnavailable
}
