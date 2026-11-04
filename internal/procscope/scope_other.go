//go:build !linux

package procscope

func nativeOps() scopeOps {
	return scopeOps{uid: func() uint32 { return 0 }}
}

func ObserveProcess(int) (ProcessIdentity, error) {
	return ProcessIdentity{}, ErrIsolation
}

func CurrentControlGroup() (string, error) {
	return "", ErrIsolation
}
