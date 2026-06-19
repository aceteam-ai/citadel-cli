//go:build tools

// Package tools pins build-time-only dependencies so `go mod tidy` keeps them
// in go.mod/go.sum. The release build runs `go run docs/gen-manpage.go` (a
// `//go:build ignore` script) to generate man pages; it imports
// github.com/spf13/cobra/doc, which transitively needs
// github.com/cpuguy83/go-md2man/v2. Because the man-gen script is excluded from
// normal builds, `go mod tidy` would otherwise strip go-md2man from go.sum and
// `build.sh --all` would fail from a clean checkout. This file is never
// compiled into any binary (the `tools` build tag is never enabled).
package tools

import (
	_ "github.com/spf13/cobra/doc"
)
