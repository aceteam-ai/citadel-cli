// Stub for Windows — single-instance detection uses Unix domain sockets
// which are not supported. All functions return safe no-op values.
//
//go:build windows

package instance

import "errors"

var errUnsupported = errors.New("single-instance detection is not supported on Windows")

type Server struct{}

func Listen(configDir string) (*Server, error) { return nil, errUnsupported }
func SocketPath(configDir string) string       { return "" }
func IsRunning(configDir string) bool          { return false }
func PID(configDir string) int                 { return 0 }
func WritePID(configDir string) error          { return nil }
func RemovePID(configDir string)               {}
func SendSIGWINCH()                            {}
func ReadPIDFile(configDir string) int         { return 0 }
func Attach(configDir string) error            { return errUnsupported }
func (s *Server) Close()                       {}
