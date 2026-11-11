//go:build !linux

package declaredsession

func spawnTerminal(Declaration, []string, []string, uint64) (holderChild, error) {
	return nil, ErrUnsupported
}

func nativeOwner() (ProcessIdentity, error) { return ProcessIdentity{}, ErrUnsupported }
