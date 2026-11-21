//go:build windows

package platform

import "syscall"

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// ConsoleProcessCount returns how many processes are attached to this
// process's console. 1 means this process is alone on it, which is what
// Explorer's double-click produces (a terminal always adds its shell). An error
// means there is no console to ask about.
func ConsoleProcessCount() (int, error) {
	var ids [4]uint32
	n, _, err := procGetConsoleProcessList.Call(uintptr(unsafePointer(&ids[0])), uintptr(len(ids)))
	if n == 0 {
		return 0, err
	}
	return int(n), nil
}
