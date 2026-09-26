//go:build windows

package network

import (
	"context"
	"fmt"
	"net"

	"github.com/tailscale/go-winio"
	"golang.org/x/sys/windows"
)

// This is the Windows primitive safesocket uses, without safesocket's
// just-started-process retry loop. A truly missing pipe stays distinguishable
// from a busy or access-denied live pipe.
func dialLocalControlRaw(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, path, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
}

// The regular safesocket pipe has a Builtin Users RW ACE. Session stop is a
// privileged local operation, so its instance at the SAME pipe name must be
// limited to the owning account, SYSTEM and Administrators. Do not rely on
// ProtectedPrefix alone: its policy varies across supported Windows releases.
func sessionPipeSDDL() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read session owner SID: %w", err)
	}
	owner := user.User.Sid.String()
	if owner == "" {
		return "", fmt.Errorf("session owner SID is empty")
	}
	return "D:P(A;;GA;;;" + owner + ")(A;;GA;;;SY)(A;;GA;;;BA)", nil
}

func listenSessionControlEndpoint(path string) (net.Listener, error) {
	sddl, err := sessionPipeSDDL()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    256 * 1024,
		OutputBufferSize:   256 * 1024,
	})
}
