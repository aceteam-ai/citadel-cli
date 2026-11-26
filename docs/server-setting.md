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

An enrolled node refuses a different server. Disconnect or log out and enroll
again to change it. The coordination endpoint still comes from enrollment;
it is not inferred from the server hostname. Existing saved private-CA origins
remain available for reconnects.

The resolved server feeds enrollment, Control Center links and terminal auth,
memory login, and private-CA persistence. `whoami` and `status` show the chosen
server, including their JSON reports. The setup-page field and installer
passthrough are separate work and are not included here.
