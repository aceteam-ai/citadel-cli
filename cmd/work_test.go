package cmd

import "testing"

func TestShellQueueName(t *testing.T) {
	tests := []struct {
		orgID string
		want  string
	}{
		{
			orgID: "550e8400-e29b-41d4-a716-446655440000",
			want:  "jobs:v1:shell:org_550e8400-e29b-41d4-a716-446655440000",
		},
		{
			orgID: "test-org-id",
			want:  "jobs:v1:shell:org_test-org-id",
		},
		{
			orgID: "",
			want:  "jobs:v1:shell:org_",
		},
	}

	for _, tt := range tests {
		got := shellQueueName(tt.orgID)
		if got != tt.want {
			t.Errorf("shellQueueName(%q) = %q, want %q", tt.orgID, got, tt.want)
		}
	}
}
