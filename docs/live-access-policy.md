# Live access policy

The worker gateway, standalone gateway and terminal entry points read the same
local permissions file for each capability request or terminal connection. A
read or parse error refuses that capability operation. Health routes keep their
existing behavior.

Attached gateway capability streams and terminal connections watch local policy
at a 250 ms interval. Disabling the capability or making its policy unreadable
ends the attachment. Terminal messages also check current console permission
before processing. Changing a legacy passcode hash ends sensitive attachments;
new requests always verify against the current passcode configuration, including
the existing vault verifier when configured. Policy watching never retries a
passcode or consumes passcode attempts.

This does not start a listener that was disabled when the process started, or
remove retained terminal state. Existing identity checks, passcode checks and
route-specific policy remain in place. An enabled capability still requires its
normal authentication.
