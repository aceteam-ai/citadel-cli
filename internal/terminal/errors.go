// internal/terminal/errors.go
package terminal

import "errors"

// Configuration errors
var (
	// ErrInvalidPort indicates the port number is out of valid range
	ErrInvalidPort = errors.New("port must be between 1 and 65535")

	// ErrInvalidMaxConnections indicates max connections is too low
	ErrInvalidMaxConnections = errors.New("max connections must be at least 1")

	// ErrInvalidIdleTimeout indicates idle timeout is too short
	ErrInvalidIdleTimeout = errors.New("idle timeout must be at least 1 minute")

	// ErrMissingOrgID indicates the organization ID is required but not set
	ErrMissingOrgID = errors.New("organization ID is required")
)

// Authentication errors
var (
	// ErrInvalidToken indicates the authentication token is invalid
	ErrInvalidToken = errors.New("invalid or expired authentication token")

	// ErrTokenExpired indicates the authentication token has expired
	ErrTokenExpired = errors.New("authentication token has expired")

	// ErrUnauthorized indicates the user is not authorized for this action
	ErrUnauthorized = errors.New("unauthorized")

	// ErrAuthServiceUnavailable indicates the auth service could not be reached
	ErrAuthServiceUnavailable = errors.New("authentication service unavailable")
)

// Rate limiting errors
var (
	// ErrRateLimited indicates the client has exceeded the rate limit
	ErrRateLimited = errors.New("rate limit exceeded, please try again later")
)

// Connection errors
var (
	// ErrMaxConnectionsReached indicates the server has reached its connection limit
	ErrMaxConnectionsReached = errors.New("maximum connections reached")

	// ErrConnectionClosed indicates the connection was closed unexpectedly
	ErrConnectionClosed = errors.New("connection closed")

	// ErrIdleTimeout indicates the connection was closed due to inactivity
	ErrIdleTimeout = errors.New("connection closed due to idle timeout")
)

// Session errors
var (
	// ErrSessionNotFound indicates the requested session does not exist
	ErrSessionNotFound = errors.New("session not found")

	// ErrSessionAlreadyExists indicates a session with that ID already exists
	ErrSessionAlreadyExists = errors.New("session already exists")

	// ErrPTYCreationFailed indicates the PTY could not be created
	ErrPTYCreationFailed = errors.New("failed to create PTY")

	// ErrShellNotFound indicates the configured shell could not be found
	ErrShellNotFound = errors.New("shell not found")
)

// Protocol errors
var (
	// ErrInvalidMessageType indicates the message type is not recognized
	ErrInvalidMessageType = errors.New("invalid message type")

	// ErrInvalidResize indicates the resize dimensions are invalid
	ErrInvalidResize = errors.New("invalid resize dimensions: cols and rows must be > 0")

	// ErrInvalidMessage indicates the message is malformed
	ErrInvalidMessage = errors.New("invalid message format")
)

// Server errors
var (
	// ErrServerNotRunning indicates the server is not currently running
	ErrServerNotRunning = errors.New("server is not running")

	// ErrServerAlreadyRunning indicates the server is already running
	ErrServerAlreadyRunning = errors.New("server is already running")
)
