package containerjobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestContainerEnrolledOrganizationBeforeLookupAndAfterLock(t *testing.T) {
	for _, org := range []string{"", otherOrg} {
		f := newFake(t)
		f.afterLock = func() { t.Fatal("unbound org reached runtime lock") }
		e := &Engine{Runtime: f, EnrolledOrg: func() string { return org }}
		if _, err := e.Run(context.Background(), Export, transferRequest()); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unbound org admitted", err)
		}
	}
	f := newFake(t)
	e := &Engine{Runtime: f}
	if _, err := e.Run(context.Background(), Usage, baseRequest()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing node authority admitted")
	}
	org := testOrg
	e.EnrolledOrg = func() string { return org }
	f.afterLock = func() { org = otherOrg }
	if _, err := e.Run(context.Background(), Stop, baseRequest()); !errors.Is(err, ErrUnavailable) || len(f.effects) != 0 {
		t.Fatal("org changed during lock admitted", err)
	}
}

func TestContainerNetworkNoneOnlyAtParserAndNativeArguments(t *testing.T) {
	for _, network := range []string{"", "none", "bridge", "host", "aceteam-net-1234abcd"} {
		r := baseRequest()
		r.QuotaBytes = 16 << 20
		r.Image = "ghcr.io/aceteam-ai/workspace:v1"
		r.CPUs = 1
		r.MemoryBytes = 1 << 30
		r.PidsLimit = 128
		r.Network = network
		raw, _ := json.Marshal(r)
		_, err := Parse(Create, raw)
		_, argsErr := createArguments(r, HostPolicy{}, true)
		allowed := network == "" || network == "none"
		if (err == nil) != allowed || (argsErr == nil) != allowed {
			t.Fatal("network policy drift", network, err, argsErr)
		}
	}
}

func TestContainerOriginalIntegersAndBothReferenceWidths(t *testing.T) {
	for _, suffix := range []string{"1234abcd", "1234abcd1234abcd1234abcd1234abcd"} {
		raw := []byte(`{"org_id":"` + testOrg + `","container_ref":"sandbox-` + suffix + `","volume_ref":"aceteam-vol-` + suffix + `","image":"ghcr.io/aceteam-ai/workspace:v1","cpus":1,"memory_bytes":9007199254740993,"pids_limit":128,"quota_bytes":16777216}`)
		r, err := Parse(Create, raw)
		if err != nil || r.MemoryBytes != 9007199254740993 {
			t.Fatal("typed integer or runtime reference changed", err)
		}
		if _, err := Parse(Create, append(raw, []byte(` {}`)...)); err == nil {
			t.Fatal("trailing JSON admitted")
		}
	}
}
