//go:build windows

package network

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSessionControlEndpointAbsentWindowsErrors(t *testing.T) {
	if !SessionControlEndpointAbsent(fmt.Errorf("pipe: %w", windows.ERROR_FILE_NOT_FOUND)) {
		t.Fatal("missing Windows pipe must classify absent")
	}
	for _, err := range []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_PIPE_BUSY, windows.WSAECONNREFUSED} {
		if SessionControlEndpointAbsent(fmt.Errorf("pipe: %w", err)) {
			t.Fatalf("non-absence Windows error classified absent: %v", err)
		}
	}
}
