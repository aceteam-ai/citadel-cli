# internal/fabricpb — vendored fabric-protocol Go

This package is a **vendored copy** of the protoc-generated Go for the fabric
node-reconcile wire contract. The canonical source of truth is the private repo
[`aceteam-ai/fabric-protocol`](https://github.com/aceteam-ai/fabric-protocol)
(`proto/aceteam/fabric/v1/*.proto`, generated Go at
`gen/go/aceteam/fabric/v1/*.pb.go`). The Go package name is `fabricv1` (the
directory is `fabricpb` to read as "fabric protobuf"; Go allows the two to
differ).

## Why vendored (not `go get`)

`fabric-protocol` is a **private** module. `go get` of a private module needs
auth that citadel-cli CI does not have yet (no deploy key configured), which
would break the build everywhere. Vendoring the generated `.pb.go` guarantees
citadel-cli builds with zero new infra. The only runtime dependency the
generated code pulls in — `google.golang.org/protobuf` — is already in `go.mod`.

Tracking issue to switch to a real go-module dependency once CI has private-repo
auth: **aceteam-ai/citadel-cli#378**.

## How to re-sync after the contract changes

When `aceteam-ai/fabric-protocol` regenerates the Go (e.g. a `.proto` change +
`buf generate`), copy the regenerated files back into this directory verbatim,
then re-add the vendor-provenance header at the top of each file:

```sh
# from a checkout of aceteam-ai/fabric-protocol with the new gen output, or via gh:
for f in node_state node_activity; do
  gh api repos/aceteam-ai/fabric-protocol/contents/gen/go/aceteam/fabric/v1/$f.pb.go \
    -q .content | base64 -d > internal/fabricpb/$f.pb.go
done
# then prepend the "// Code generated from aceteam-ai/fabric-protocol; DO NOT EDIT."
# provenance header (see the top of each existing file) and run `go build ./...`.
```

Do **not** hand-edit the `.pb.go` files for anything other than the provenance
header — they are machine-generated.

## Contract versioning

The wire contract version is `protocol.FabricProtocolVersion`
(`internal/protocol/protocol.go`, aceteam-ai/citadel-cli#363). Bump that constant
on any breaking change to the proto, and keep this vendored copy in sync.
