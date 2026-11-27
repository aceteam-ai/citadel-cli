//go:build !linux && !darwin && !windows

package jobs

import "time"

func fileBirthTime(string) (time.Time, bool) {
	return time.Time{}, false
}
