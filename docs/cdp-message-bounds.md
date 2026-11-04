# CDP message and screenshot bounds

Refs #1386. This standalone source hardening changes the existing shared
cobrowse/render CDP path when that source is executed or released. It is not
an inactive renderer, a scene implementation or template registration. The
new fixed ceiling can refuse large replies previously accepted without a cap.

`internal/cobrowsestream.dialCDP` uses a fixed 64 MiB incoming WebSocket message
limit. `SetReadLimit` is installed after dialing and before the read goroutine
can make its first `ReadMessage` call. Gorilla counts continuation frames
together, so fragmenting a message does not create fresh capacity. Compression
is explicitly disabled on a local copy of the default dialer because Gorilla's
limit counts compressed payload bytes before decompression. The pinned dialer
can accept an unsolicited compression response despite making no offer, so
every extension response header is also refused, including empty or duplicate
fields. The connection is closed before applying the read limit or starting
the reader. Other dialer behavior and the shared default dialer are preserved.
There is no public,
configuration or CLI option to disable or raise the limit; a private bounded
smaller-limit seam serves source fixtures.

A 32 MiB PNG occupies 44,739,244 base64 bytes, leaving room for the CDP envelope
inside 64 MiB. `chromiumRenderPage.CapturePNG` passes received data through
`decodePapercraftScreenshot` with the existing 32 MiB PNG ceiling. It checks raw
encoded byte length before base64 decoding, then computes a padding-aware
decoded-length preflight. Valid exact-limit data ending in `=` or `==` is not
rejected merely because `DecodedLen` overestimates padded output. The preflight
also refuses over-limit output that shares the encoded length of valid data.
CR/LF retains standard base64 semantics while consuming the raw encoded budget.
Actual decoded length is checked afterward as well. `DecodeString` may reserve
up to two extra rounding bytes; this is not a strict exact-allocation promise.
PNG signature checks in the render loop remain in place.

The WebSocket limit bounds one incoming message, not total process memory,
concurrent pending commands, JSON expansion/copies, frame histories or whole-job
resident data. Target discovery still performs an unbounded HTTP `/json`
`io.ReadAll`; a separate required boundary remains before any future claim of
end-to-end CDP memory confinement. This patch does not supply #1386's full
renderer/scene ABI, WebGL support, sandbox mode, shared analysis, output policy
or resource-budget acceptance. It does not change browser launch flags.

Prepared source fixtures use tiny fake HTTP/WebSocket endpoints and pure
decoding inputs, with no Chromium or ffmpeg. They exercise the first oversized
message while the server stays open, aggregate fragmented size, accepted
exact/below-limit messages, pending-command refusal, compression pinning and
unsolicited-extension refusal through valid raw WebSocket upgrades (including
an empty first header followed by compression), plus padding-aware screenshot
checks before decoding. An actual `CapturePNG` call
also consumes a fake CDP response. These fixtures are intended for normal
remote GitHub CI; no local candidate parser, formatter, tests, build, socket,
browser or native execution accompanied this source proposal.
