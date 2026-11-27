# Descriptor-based session identifiers

Session inventory keeps an explicit argv thread identifier when present. On
Linux it can otherwise derive a thread identifier from open descriptor links
in the recognized harness process and its same-user descendants in that pane.

Only default transcript locations under the current OS account's home are recognized:
project session files for Claude and dated rollout files for Codex. Environment
home overrides do not select another account's transcript roots. The observer
reads link metadata, never transcript contents, and publishes only a validated
UUID. Custom transcript roots and other platforms retain argv-only discovery.

Real and effective UID and process birth must match before and after descriptor
observation. Existing pane, TTY and managed-terminal exclusions still apply.
Conflicting identifiers, observation errors and bounds withhold the fallback
and mark `thread_fds` unavailable. Each process is limited to 256 descriptors,
and one inventory observes at most 256 process descriptor directories under its
existing deadline. A missing ID is not authority to attach or resume a session.

Tests use synthetic process metadata and owned fake procfs trees. No live
process, descriptor or transcript inspection is needed to run those tests.
