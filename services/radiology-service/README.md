# Radiology module — Orthanc-only C1

This inactive source slice defines a node-local DICOM receiver. It does not
install or activate a service, grant Files permission, send frames to a provider,
create a trigger key, run a bridge, or expose a viewer. Synthetic or public
de-identified studies only; real PHI requires the sovereign rollout and explicit
owner sign-off in [the design of record](https://github.com/aceteam-ai/aceteam/issues/11236#issuecomment-6153389652).
Not for diagnostic use.

## Configuration

The first-party catalog module is named `radiology`. Its source manifest and
compose are mirrored in `citadel-cli/services/radiology-service` and
`citadel-services/services/radiology`; it is not an embedded inference engine.

| Setting | Default | Meaning |
| --- | --- | --- |
| `RADIOLOGY_AET` | `CITADEL` | Receiver AE title; sending PACS must register it |
| `PACS_AET` | required | Exactly one allowed sending peer AE title |
| `PACS_HOST` | required | Peer source host as observed by Orthanc |
| `PACS_PORT` | `4242` | Peer DICOM port |
| `RADIOLOGY_DICOM_PORT` | `4242` | Host LAN DICOM publish |
| `RADIOLOGY_HTTP_PASSWORD` | generated | Sticky password for fixed HTTP user `orthanc` |
| `RADIOLOGY_STABLE_AGE` | `60` | Study stable-age seconds |

Peer values occupy fixed upstream object paths (`DicomModalities.Pacs.AET`,
`Host`, `Port`); they are never interpolated into a JSON map. Quotes or object
fragments cannot create another peer or change a sibling hardening setting.
The upstream configurator parses valid JSON values, otherwise preserves strings;
Orthanc's strict object decoder rejects wrong scalar types and invalid ports,
rather than falling back to an open peer list. Plain invalid AE/host strings
remain invalid names, not config instructions. Use a valid AE/host and TCP port.
The configured peer may echo and store only; query/retrieve and commitment are
disabled pending their later slices. Register the receiver
AE and verify the peer source address with the operator before live acceptance.
DICOM AE/host checks are not cryptographic authentication; restrict the DICOM
LAN listener to the approved test peers using the site's network policy.

`services/ports.go` reserves default DICOM 4242 and injects HTTP 8216 through
`CITADEL_RADIOLOGY_HOST_PORT`. HTTP is always published on **127.0.0.1**, not LAN.
The manifest's DICOM port-conflict preflight checks the default only; the operator
must reserve/check any configured nondefault DICOM port. A custom HTTP port is
not supported by this slice because the host health declaration targets 8216.

The existing installer generates the HTTP secret once and reuses its 0600 module
env file on reconcile/update. Never print that env file, the compose's rendered
configuration, or container environment; those contain the password. Credentials
are not reused from the node device token. Startup verbose/trace configuration is
explicitly disabled. The container probe uses the password only from its own
environment, never from expanded command-line arguments, and prints no body,
credentials or exception text.

Raw DICOM and index are under `~/.citadel-cli/radiology/orthanc`, mounted at
`/var/lib/orthanc` with separate `storage` and `index` subdirectories. They are
outside the workspace and must **never** be added as a Files authorized root.
There is no workspace bind, frame converter, retention/recycling worker, or
push-back in C1; capacity and manual test-data cleanup remain operator gates.
Bridge configuration, sentinel, retention policy and workspace paths belong to
the later bridge slice, not fabricated no-op settings here.

## Health and viewer gates

The in-container healthcheck authenticates `GET /system`, requires HTTP 200 and
a bounded recognized JSON object, rejects proxies and redirects, and is silent
on failure. This verifies the HTTP API, not database read/write correctness.
The manifest's `/system` check is **transport liveness only**: existing
`catalog.ProbeHealth` intentionally considers 401/403 an answering server. It
does not prove the password works. C8 adds strict authenticated host readiness;
no general catalog-auth framework or auth weakening is introduced here.

OHIF and Orthanc Explorer 2 are hard-disabled. C1 intentionally offers no
`RADIOLOGY_VIEWER=true` setting. Enabling a viewer is not supported until a
separately reviewed node-authenticated `EXPOSE_SET` route and a fixed
**NOT FOR DIAGNOSTIC USE** banner are implemented. No gateway declaration or
automatic exposure is present. The Orthanc instance name also carries the banner
but is not a substitute for the gated viewer UI.

## Pin and provenance (metadata checked 2026-10-13)

The unmodified upstream image is Orthanc Team **26.9.1**, multi-architecture index
`sha256:d2705f2c56547e55ce9bc2250fd22d05d326493b3f3d9fa191ecb7db8ca6b5a2`.
The publisher's [tag metadata](https://hub.docker.com/v2/repositories/orthancteam/orthanc/tags/26.9.1)
reports these Linux platform manifests:

- amd64: `sha256:3a7b0447b09a0e88ba78a091d76f2c64693c4c751fe148ea859c0e852fc648e2`
- arm64: `sha256:ee6aeb31798b6ff027c11e4f709b2d5d733574588352789e3c6f81876cd786f1`

The publisher's [image metadata](https://hub.docker.com/v2/repositories/orthancteam/orthanc/tags/26.9.1/images?page_size=10)
contains Python3 installation and `COPY` histories for `libOrthancDicomWeb.so`
and `libOrthancOHIF.so` on both architectures. This is metadata evidence, not
proof of runtime plugin loading or native acceptance; no image was pulled or run.
The [upstream release notes](https://github.com/orthanc-server/orthanc-builder/blob/master/release-notes-docker-images.md)
and [Orthanc Docker documentation](https://orthanc.uclouvain.be/book/users/docker-orthancteam.html)
describe the environment configuration and `ORTHANC_PASSWORD` support. DICOMweb
is enabled; the bundled OHIF plugin is deliberately not activated.
The upstream [configurator](https://github.com/orthanc-server/orthanc-builder/blob/master/docker/orthanc/configurator.py),
[environment-key helper](https://github.com/orthanc-server/orthanc-builder/blob/master/docker/orthanc/helpers.py)
and [Orthanc 1.13.0 configuration](https://orthanc.uclouvain.be/hg/orthanc/file/Orthanc-1.13.0/OrthancServer/Resources/Configuration.json)
define the nested peer paths and object-form permission controls.

The module's own manifest/configuration are Apache-2.0. Orthanc core is GPL and
some bundled plugins are AGPL; these are upstream components, not relicensed
Apache code. Their license/distribution obligations must be reviewed before
distribution or marketplace activation.

## Acceptance still required

Hermetic source tests check the real manifest schema, pin, hardening, private
storage, port registry, disabled viewer and silent bounded probe contract. They
do not install the module or validate DICOM negotiation.

After separate deployment authorization on an isolated test node, acceptance
must verify authenticated HTTP works and unauthenticated HTTP is denied, plugins
load at the pin, known-peer synthetic C-STORE succeeds, unknown AE/host and wrong
called AE are rejected, and raw storage remains outside file-read roots. Verify
secret persistence across reconcile/update without displaying it. Preserve that
gate in #1303; do not claim it complete from metadata or source CI alone.
