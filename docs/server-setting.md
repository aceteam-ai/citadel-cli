# Control-plane server

Use `citadel --server server.example.test init` to choose a control plane.
A bare host means HTTPS. `CITADEL_SERVER` provides the same setting for an
invocation. The hidden `--auth-service` flag and `CITADEL_AUTH_HOST` environment
variable remain compatibility aliases.

Selection order is an explicit flag, environment, saved `api_base_url`, then
the built-in default. Within environment settings, `CITADEL_SERVER` wins over
the legacy alias. Two explicit flags must agree. URLs must be origins, without
credentials, paths, queries or fragments. Plain HTTP is accepted only for
loopback development endpoints.

An enrolled node refuses a different server. `citadel logout` and the Control
Center network-disconnect action alone do not clear the saved device enrollment
or private-CA binding, so neither is a server-reset procedure.

For a device enrollment **without a persisted private-CA binding**, use
`citadel --auth-service https://current-server.example.test local disconnect`,
using the current server shown by `whoami`, and verify that every step succeeds
before running `citadel --server new-server.example.test init`. Name the current
server for disconnect, not the new destination. Local-mode setup/disconnect
still uses the compatibility flag directly in this CLI slice; the setup-path
server-selector wiring is separate work. The disconnect flow stops the
per-user worker, removes its autostart, logs out and removes the enrollment's
device credentials from both current and legacy locations. On systems with a
separately managed worker, stop that worker as directed by the disconnect
output. Do not proceed after a held or failed step.

Switching an enrollment with a persisted private-CA binding is not supported by
this slice. It remains refused after device disconnect. An explicit, separately
reviewed binding-reset flow is still required; do not delete identity, permission
or saved-login directories to work around it.

The coordination endpoint still comes from enrollment; it is not inferred from
the server hostname. Existing saved private-CA origins remain available for
reconnects. HPC launch/run require an explicit server flag or environment
setting, use the same resolver, and do not fall back to host enrollment.

The resolved server feeds enrollment, Control Center links and terminal auth,
memory login, and private-CA persistence. `whoami` and `status` show the chosen
server, including their JSON reports. The setup-page field and installer
passthrough are separate work and are not included here.
