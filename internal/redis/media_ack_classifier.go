package redis

import _ "embed"

// mediaACKClassifier is a pure retention predicate, not a registered Redis
// script or execution authority. It is not wired to ACK/deletion until original
// queue/target ownership, atomic deletion and paired API/WS gates are resolved.
//
//go:embed media_ack_classifier.lua
var mediaACKClassifier string
