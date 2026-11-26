//go:build !linux && !darwin

package sessionsearch

import (
	"context"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/laneinventory"
)

func ownerSupported(int) bool                                 { return false }
func owned(os.FileInfo, int) bool                             { return false }
func openOwned(*os.Root, string, int, bool) (*os.File, error) { return nil, ErrUnavailable }
func Native(context.Context, Options, []laneinventory.Pane) (Report, error) {
	return Report{}, ErrUnavailable
}
func (*search) hookName(*group) {}
