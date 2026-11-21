//go:build windows

package botprofile

import "errors"

// NodeShareFile is the node share's file name inside the store dir.
const NodeShareFile = "node-share.key"

// ErrUnsupportedPlatform: Windows has no mode bits to check and no DACL check
// is built for this store yet, so it fails closed rather than keep a key file
// it cannot prove private. Bot profiles run in the Linux container runtime.
var ErrUnsupportedPlatform = errors.New("botprofile: node share is not supported on this platform")

func loadOrCreateNodeShare(string) ([]byte, error) { return nil, ErrUnsupportedPlatform }
