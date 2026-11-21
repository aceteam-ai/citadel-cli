package localcobrowse

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func fixedResolver(m map[string][]string) Resolver {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := m[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		out := make([]netip.Addr, 0, len(ips))
		for _, s := range ips {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestURLPolicy(t *testing.T) {
	policy := NewURLPolicy(fixedResolver(map[string][]string{
		"public.test":   {"203.0.113.10"},
		"mixed.test":    {"203.0.113.10", "10.1.2.3"},
		"rebind.test":   {"127.0.0.1"},
		"v6public.test": {"2a01:4f8::1"},
		"v6ula.test":    {"fd00::1"},
	}), func() []int { return []int{43117} })

	allowed := []string{
		"https://public.test/",
		"http://public.test:8080/path?q=1",
		"https://v6public.test/",
		"about:blank",
		"http://203.0.113.10/",
		"https://[2a01:4f8::1]/",
	}
	for _, u := range allowed {
		if err := policy(context.Background(), u); err != nil {
			t.Errorf("%s: unexpectedly refused: %v", u, err)
		}
	}
	refused := []string{
		"file:///C:/Windows/win.ini",
		"data:text/html,hi",
		"javascript:alert(1)",
		"chrome://settings",
		"edge://settings",
		"devtools://devtools/x",
		"view-source:https://public.test/",
		"ftp://public.test/",
		"http://localhost/",
		"http://app.localhost:3000/",
		"http://127.0.0.1:43117/",
		"http://public.test:43117/",
		"http://127.0.0.1/",
		"http://127.1/",
		"http://2130706433/",
		"http://0x7f000001/",
		"http://0x7f.1/",
		"http://0177.0.0.1/",
		"http://0.0.0.0/",
		"http://0/",
		"http://[::1]/",
		"http://[::ffff:127.0.0.1]/",
		"http://[::ffff:7f00:1]/",
		"http://[::127.0.0.1]/",
		"http://[::7f00:1]/",
		"http://[::a00:1]/",
		"http://[64:ff9b::7f00:1]/",
		"http://[fe80::1]/",
		"http://10.0.0.5/",
		"http://172.16.4.4/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.10/",
		"http://224.0.0.1/",
		"http://255.255.255.255/",
		"http://mixed.test/",
		"http://rebind.test/",
		"http://v6ula.test/",
		"http://unresolvable.test/",
		"http://user:pass@public.test/",
		"http://1.2.3.999/",
		"not a url at all",
	}
	for _, u := range refused {
		if err := policy(context.Background(), u); !errors.Is(err, ErrNavigationRefused) {
			t.Errorf("%s: want refusal, got %v", u, err)
		}
	}
}

func TestParseBrowserHost(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":   "127.0.0.1",
		"127.1":       "127.0.0.1",
		"2130706433":  "127.0.0.1",
		"0x7f.1":      "127.0.0.1",
		"0177.0.0.1":  "127.0.0.1",
		"10.1":        "10.0.0.1",
		"192.168.257": "192.168.1.1",
		"0":           "0.0.0.0",
	}
	for in, want := range cases {
		got, ok := parseBrowserHost(in)
		if !ok || got.String() != want {
			t.Errorf("%s: got %v ok=%v want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"example.test", "1.2.3.4.5", "256.1.1.1"} {
		if _, ok := parseBrowserHost(in); ok {
			t.Errorf("%s should not parse as an address", in)
		}
	}
}
