// The local and remote runners share one destination-pinning proxy.
package localcobrowse

import (
	"context"
	"github.com/aceteam-ai/citadel-cli/internal/browseregress"
	"net"
)

type EgressGuard = browseregress.EgressGuard

var ErrEgressRefused = browseregress.ErrEgressRefused
var ErrPrivateNetworkApprovalRequired = browseregress.ErrPrivateNetworkApprovalRequired

func StartEgressGuard(resolve Resolver, dial func(context.Context, string, string) (net.Conn, error)) (*EgressGuard, error) {
	return browseregress.StartEgressGuard(resolve, dial)
}
