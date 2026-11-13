package cmd

import "testing"

func TestReceiptNodeIDOrder(t *testing.T) {
	cases := []struct{ fabric, routing, want string }{
		{"42", "gpu-box", "42"},
		{"", "gpu-box", "gpu-box"},
		{"  ", " gpu-box ", "gpu-box"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := receiptNodeID(c.fabric, c.routing); got != c.want {
			t.Errorf("receiptNodeID(%q,%q)=%q want %q", c.fabric, c.routing, got, c.want)
		}
	}
}
