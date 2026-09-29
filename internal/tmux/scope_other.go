//go:build !linux

package tmux

func persistentSessionCommand(_ string, command []string) []string {
	return command
}
