package trust

import "testing"

func TestCheckWork(t *testing.T) {
	cases := []struct {
		name   string
		action string
		units  map[string]int64
		wantOK bool
	}{
		{
			name:   "embed_served non-zero tokens and bytes OK",
			action: "embed_served",
			units:  map[string]int64{"total_tokens": 384, "request_bytes": 1024, "response_bytes": 4096},
			wantOK: true,
		},
		{
			name:   "embed_served zero tokens flagged",
			action: "embed_served",
			units:  map[string]int64{"total_tokens": 0, "request_bytes": 1024, "response_bytes": 4096},
			wantOK: false,
		},
		{
			name:   "embed_served zero bytes flagged",
			action: "embed_served",
			units:  map[string]int64{"total_tokens": 10, "request_bytes": 0, "response_bytes": 0},
			wantOK: false,
		},
		{
			name:   "index with files and chunks OK",
			action: "index",
			units:  map[string]int64{"files_indexed": 3, "chunks_upserted": 12},
			wantOK: true,
		},
		{
			name:   "index with files but zero chunks flagged",
			action: "index",
			units:  map[string]int64{"files_indexed": 3, "chunks_upserted": 0},
			wantOK: false,
		},
		{
			name:   "index no-op reindex OK",
			action: "index",
			units:  map[string]int64{"files_indexed": 0, "chunks_upserted": 0, "files_skipped": 10},
			wantOK: true,
		},
		{
			name:   "negative unit flagged for any action",
			action: "index",
			units:  map[string]int64{"files_indexed": -1},
			wantOK: false,
		},
		{
			name:   "unknown action with sane units OK",
			action: "extract",
			units:  map[string]int64{"pages": 5},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckWork(tc.action, tc.units)
			if got.OK != tc.wantOK {
				t.Fatalf("CheckWork(%q,%v).OK = %v (reason %q), want %v", tc.action, tc.units, got.OK, got.Reason, tc.wantOK)
			}
			if !got.OK && got.Reason == "" {
				t.Error("a failing check must carry a reason")
			}
		})
	}
}
