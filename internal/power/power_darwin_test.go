//go:build darwin

package power

import "testing"

func TestParsePmsetBatt(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want Source
	}{
		{
			name: "ac power",
			out:  "Now drawing from 'AC Power'\n -InternalBattery-0 (id=12345)\t100%; charged; 0:00 remaining present: true\n",
			want: SourceAC,
		},
		{
			name: "battery power",
			out:  "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=12345)\t82%; discharging; 4:11 remaining present: true\n",
			want: SourceBattery,
		},
		{
			name: "desktop no battery",
			out:  "Now drawing from 'AC Power'\n",
			want: SourceAC,
		},
		{
			name: "garbage",
			out:  "unexpected output",
			want: SourceUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parsePmsetBatt(c.out); got != c.want {
				t.Errorf("parsePmsetBatt() = %v, want %v", got, c.want)
			}
		})
	}
}
