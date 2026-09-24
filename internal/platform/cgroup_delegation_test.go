package platform

import (
	"errors"
	"reflect"
	"testing"
)

func TestEvaluateCgroupDelegation(t *testing.T) {
	health := evaluateCgroupDelegation([]string{"pids", "cpu", "memory", "cpu"}, "/controllers", []byte("io pids memory cpu\n"), nil)
	if !health.Applicable || !health.OK {
		t.Fatalf("health = %+v, want applicable and OK", health)
	}
	want := []string{"cpu", "io", "memory", "pids"}
	if !reflect.DeepEqual(health.Controllers, want) {
		t.Fatalf("controllers = %v, want %v", health.Controllers, want)
	}
}

func TestEvaluateCgroupDelegationFailsClosed(t *testing.T) {
	health := evaluateCgroupDelegation([]string{"cpu", "memory", "pids"}, "/controllers", []byte("cpu io\n"), nil)
	if health.OK || !reflect.DeepEqual(health.Missing, []string{"memory", "pids"}) {
		t.Fatalf("health = %+v, want missing memory and pids", health)
	}

	unreadable := evaluateCgroupDelegation([]string{"memory"}, "/controllers", nil, errors.New("denied"))
	if unreadable.OK || !reflect.DeepEqual(unreadable.Missing, []string{"memory"}) {
		t.Fatalf("unreadable health = %+v, want fail closed", unreadable)
	}
}
