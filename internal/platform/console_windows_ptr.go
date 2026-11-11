//go:build windows

package platform

import "unsafe"

func unsafePointer(p *uint32) unsafe.Pointer { return unsafe.Pointer(p) }
