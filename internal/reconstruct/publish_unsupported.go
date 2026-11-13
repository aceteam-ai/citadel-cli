//go:build !linux

package reconstruct

import (
	"context"
	"fmt"
)

func publishDirectory(ctx context.Context, _, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("reconstruct: create-only bundle publication requires the supported Linux GPU container")
}
