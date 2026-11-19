package declaredsession

import "context"

// ContainerVerifier proves, from the engine's side, that the container running
// the session is the one that wrote the saved state: it must be running and its
// incarnation label must equal the saved incarnation. The holder cannot prove
// that about itself, and host process observation cannot see across the PID
// namespace, so this is the container analogue of the scope observation.
type ContainerVerifier func(ctx context.Context, name, incarnation string) error

// DialContainerHolder returns an authenticated client to a container holder.
// The socket protocol is the one the host holder speaks, so `citadel attach`
// and the attach loop are unchanged. What differs is who vouches for the peer:
//
//   - the socket itself is the same-UID, 0600, no-symlink socket in the host
//     session dir that DialHolder already requires;
//   - the peer process seen from the host must be the same uid as the saved
//     holder (its PID is the container's PID 1, not a host PID);
//   - hello, saved state and live state must agree on incarnation, scope,
//     declaration revision and the container guarantee;
//   - the engine must confirm that incarnation is the running container.
func DialContainerHolder(ctx context.Context, session *SessionStore, verify ContainerVerifier) (*Client, error) {
	if verify == nil {
		return nil, ErrPeer
	}
	saved, present, err := session.ReadState()
	if err != nil || !present || saved.Guarantee != ContainerGuarantee || saved.Scope.Manager != "container" {
		return nil, ErrPeer
	}
	conn, pid, err := dialHolderSocket(ctx, session)
	if err != nil {
		return nil, err
	}
	client := &Client{framed: &framedConn{conn: conn}}
	hello, err := client.framed.read(ctx, operationWait)
	peer, peerErr := observeHolderPeer(pid)
	if err != nil || peerErr != nil || hello.Op != "hello" || hello.Hello == nil || !validContainerHello(*hello.Hello, saved, session.Paths().Name, peer) {
		_ = conn.Close()
		return nil, ErrPeer
	}
	client.hello = *hello.Hello
	if verify(ctx, saved.Name, saved.Incarnation) != nil {
		_ = client.Close()
		return nil, ErrPeer
	}
	live, err := client.Status(ctx)
	if err != nil || live.Incarnation != saved.Incarnation || live.Guarantee != ContainerGuarantee || live.DeclarationRevision != saved.DeclarationRevision || live.Scope != saved.Scope {
		_ = client.Close()
		return nil, ErrPeer
	}
	return client, nil
}

func validContainerHello(hello Hello, saved State, name string, peer ProcessIdentity) bool {
	return hello.ProtocolVersion == ProtocolVersion && saved.ProtocolVersion == ProtocolVersion &&
		hello.Name == name && saved.Name == name &&
		validIncarnation(hello.Incarnation) && hello.Incarnation == saved.Incarnation &&
		saved.Guarantee == ContainerGuarantee && saved.Scope.Manager == "container" && hello.Scope == saved.Scope &&
		hello.Holder == saved.Holder && hello.Holder.PID == 1 && hello.Holder.UID != 0 && hello.Holder.UID == peer.UID &&
		hello.DeclarationRevision == saved.DeclarationRevision && hello.BinaryVersion == saved.BinaryVersion
}
