//go:build linux

package hpc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// proc tables that list TCP sockets with their owning uid.
var procTCPTables = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// ListenerUIDs returns the owner uid of every LISTEN socket on port, read from
// the given /proc/net/tcp-format contents. Any local address counts: a
// wildcard listener from another user is as foreign as a loopback one.
func ListenerUIDs(tables []string, port int) []int {
	var uids []int
	for _, content := range tables {
		for i, line := range strings.Split(content, "\n") {
			if i == 0 {
				continue // header
			}
			f := strings.Fields(line)
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid ...
			if len(f) < 8 || f[3] != "0A" {
				continue
			}
			colon := strings.LastIndexByte(f[1], ':')
			if colon < 0 {
				continue
			}
			p, err := strconv.ParseUint(f[1][colon+1:], 16, 32)
			if err != nil || int(p) != port {
				continue
			}
			uid, err := strconv.Atoi(f[7])
			if err != nil {
				continue
			}
			uids = append(uids, uid)
		}
	}
	return uids
}

// CheckListenerOwner returns a ListenerCheck that requires every listener on
// port to belong to uid. It reads /proc/net/tcp and tcp6; a table that cannot
// be read is an error rather than a pass.
func CheckListenerOwner(port, uid int) ListenerCheck {
	return func() (bool, error) {
		var tables []string
		for _, path := range procTCPTables {
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) && strings.HasSuffix(path, "tcp6") {
					continue
				}
				return false, fmt.Errorf("cannot verify the model server's owner: %w", err)
			}
			tables = append(tables, string(data))
		}
		return checkOwners(tables, port, uid)
	}
}

func checkOwners(tables []string, port, uid int) (bool, error) {
	uids := ListenerUIDs(tables, port)
	for _, u := range uids {
		if u != uid {
			return true, fmt.Errorf("port %d is held by another user (uid %d), not the model server; refusing to use it", port, u)
		}
	}
	return len(uids) > 0, nil
}
