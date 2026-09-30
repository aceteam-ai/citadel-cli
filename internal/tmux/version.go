package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinimumVersion is the oldest tmux release that implements every format
// primitive used by Citadel's identity-bound renew, attach, and reap commands.
// tmux 2.4 added #{==}; tmux 2.6 added #{&&}. if-shell -F predates both.
const MinimumVersion = "2.6"

// ErrTmuxVersionUnsupported means a tmux executable was found but cannot safely
// evaluate Citadel's atomic ownership guards.
var ErrTmuxVersionUnsupported = errors.New("tmux version is unsupported")

const versionProbeTimeout = 2 * time.Second

var tmuxVersionPattern = regexp.MustCompile(`^tmux ([0-9]+)\.([0-9]+)(?:[a-z].*)?$`)

type tmuxVersion struct {
	major int
	minor int
}

func parseTmuxVersion(raw string) (tmuxVersion, error) {
	match := tmuxVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return tmuxVersion{}, fmt.Errorf("%w: unrecognized output %q", ErrTmuxVersionUnsupported, strings.TrimSpace(raw))
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return tmuxVersion{}, fmt.Errorf("%w: parse major version: %v", ErrTmuxVersionUnsupported, err)
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return tmuxVersion{}, fmt.Errorf("%w: parse minor version: %v", ErrTmuxVersionUnsupported, err)
	}
	return tmuxVersion{major: major, minor: minor}, nil
}

func supportedTmuxVersion(raw string) bool {
	got, err := parseTmuxVersion(raw)
	if err != nil {
		return false
	}
	want, _ := parseTmuxVersion("tmux " + MinimumVersion)
	return got.major > want.major || (got.major == want.major && got.minor >= want.minor)
}

// SupportsVersion reports whether a release version such as "3.4" satisfies
// MinimumVersion. It is exported for the managed-artifact source gate.
func SupportsVersion(version string) bool {
	return supportedTmuxVersion("tmux " + version)
}

func probeTmuxVersion(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-V").CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %s -V timed out", ErrTmuxVersionUnsupported, bin)
		}
		return fmt.Errorf("%w: %s -V failed: %v: %s", ErrTmuxVersionUnsupported, bin, err, strings.TrimSpace(string(out)))
	}
	version, err := parseTmuxVersion(string(out))
	if err != nil {
		return err
	}
	want, _ := parseTmuxVersion("tmux " + MinimumVersion)
	if version.major < want.major || (version.major == want.major && version.minor < want.minor) {
		return fmt.Errorf("%w: found %d.%d, require >= %s for atomic ownership guards", ErrTmuxVersionUnsupported, version.major, version.minor, MinimumVersion)
	}
	return nil
}

// ValidateBinary executes bin with -V and enforces MinimumVersion.
func ValidateBinary(bin string) error {
	return probeTmuxVersion(bin)
}
