# CDP target discovery bounds

Refs #1386 and [the accepted shared-scene revision 2](https://github.com/aceteam-ai/aceteam/issues/11830#issuecomment-6371178578), section 6.

The shared CDP transport's HTTP target probe requests `/json` from the selected
IPv4 loopback debug port. It retains the existing five-second client timeout and
chooses the first page with a nonempty debugger URL, after validating the entire
reply. Additive target fields remain compatible. This source affects existing
cobrowse and render callers; it is not an inactive renderer registration.

The complete response may contain at most 64 KiB of JSON, including whitespace,
and at most 64 target entries, including non-page and null entries. An oversized
declared length is refused before reading. An absent or understated length does
not remove the streaming bound: a limit reader admits at most 65,537 bytes and
refuses overflow before constructing a JSON decoder. The decoder processes one
target at a time and refuses before decoding a sixty-fifth entry. It does not
materialize an unbounded target slice.

Only HTTP 200 is accepted. Redirects are returned without following them, then
refused as unsuccessful discovery. The probe copies the HTTP client before
installing this policy, retaining its timeout and transport. Responses are
closed on success and refusal. Error bodies, raw parser errors and transport
error text are not exposed. A valid first page cannot hide excess targets,
invalid later entries, an incomplete array or trailing JSON.

Prepared fake RoundTripper and reader fixtures cover exact/over byte and target
limits, declared versus streamed lengths, status and redirect refusal, complete
JSON validation, first-page/additive compatibility, request deadlines, body
closure and read/transport failures. They use no socket, browser or native
process. Local candidate parsing, formatting, tests and builds remain held;
normal GitHub CI must verify the published source. Fixture preparation is not a
passing-test claim.

This is separate from #1455's WebSocket-message and screenshot guards. It does
not implement aggregate pending-command/JSON memory accounting, authorize a
debugger URL, pin the browser identity, change the scene artifact or launch
Chromium. Range/PCM transport, sandbox/software-WebGL proof, sixty native
stamped captures, MP4 output, registration and live acceptance remain open.
No native execution, SQL, deployment, merge or undraft is included.
