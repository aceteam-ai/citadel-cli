//go:build !windows

// internal/platform/cobrowse_macos_reap.go
//
// The macOS source for reapProfileUsers (cobrowse_profile_helpers_other.go),
// kept untagged by OS so its parsing runs on Linux CI.
package platform

import "strings"

// listMacReapProcsFrom turns `ps -ww -axo pid=,uid=,command=` output into the
// processes naming a profile (a start time is read only for those). A failed ps
// is an error, never an empty list.
func listMacReapProcsFrom(ps func() (string, error), start func(pid int) (int64, error)) ([]reapProc, error) {
	out, err := ps()
	if err != nil {
		return nil, err
	}
	var res []reapProc
	for _, r := range parseMacPS(out) {
		if !strings.Contains(r.cmd, "--user-data-dir=") && !strings.Contains(r.cmd, "--database=") {
			continue
		}
		st, err := start(r.pid)
		if err != nil {
			continue // exited since the listing
		}
		res = append(res, reapProc{pid: r.pid, start: uint64(st), uid: r.uid, args: strings.Fields(r.cmd), cmd: r.cmd})
	}
	return res, nil
}
