package containerjobs

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func TestContainerCapabilityURLsAndPrivateAddressRefusal(t *testing.T) {
	for _, raw := range []string{"http://blob.example.invalid/x", "https://user:pass@blob.example.invalid/x", "https://blob.example.invalid/x#fragment", "https://blob.example.invalid:80/x", "file:///tmp/x"} {
		if validURL(raw) {
			t.Fatal("unsafe capability URL accepted")
		}
	}
	for _, raw := range []string{"0.0.0.0", "127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.58", "198.51.100.1", "224.0.0.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1", "2001:db8::1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("private/special destination accepted", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("public control refused", raw)
		}
	}
}

func TestContainerCapabilityDialChecksAllAnswersAndPinsNumericDestination(t *testing.T) {
	for _, addresses := range [][]string{{"1.1.1.1"}, {"1.1.1.1", "127.0.0.1"}, {"1.1.1.1", "100.64.0.58"}, {"8.8.8.8"}} {
		t.Run(strings.Join(addresses, "-"), func(t *testing.T) {
			var dialed []string
			tr := capabilityTransport(func(context.Context, string) ([]net.IPAddr, error) {
				var result []net.IPAddr
				for _, s := range addresses {
					result = append(result, net.IPAddr{IP: net.ParseIP(s)})
				}
				return result, nil
			}, func(_ context.Context, _ string, addr string) (net.Conn, error) {
				dialed = append(dialed, addr)
				return nil, errors.New("synthetic dial")
			}, func() ([]net.Addr, error) {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("8.8.8.8"), Mask: net.CIDRMask(32, 32)}}, nil
			})
			_, _ = tr.client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "blob.example.invalid:443")
			if len(addresses) == 1 && addresses[0] == "1.1.1.1" {
				if len(dialed) != 1 || dialed[0] != "1.1.1.1:443" {
					t.Fatal("DNS name redialed or public control blocked", dialed)
				}
			} else if len(dialed) != 0 {
				t.Fatal("mixed/private/self destination dialed", dialed)
			}
		})
	}
}
