package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type workPlatformRecoveryFixture struct {
	bindings    int
	redial      func()
	idleRestart func(func()) bool
}

func (f *workPlatformRecoveryFixture) SetPlatformRecovery(redial func(), idleRestart func(func()) bool) {
	f.bindings++
	f.redial, f.idleRestart = redial, idleRestart
}

func isolateWorkPlatformRecovery(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		path := filepath.Join(root, key)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("LAUNCH_JOBKEY_LABEL", "")
	t.Setenv("XPC_SERVICE_NAME", "")
}

func TestWorkPlatformRecoveryDoesNotBindRedisMode(t *testing.T) {
	isolateWorkPlatformRecovery(t)
	t.Setenv("INVOCATION_ID", "fixture-systemd-unit")
	monitor := &workPlatformRecoveryFixture{}
	bindWorkPlatformRecovery(false, monitor, []func(){func() { t.Error("Redis mode redialed a platform client") }}, func(func()) bool {
		t.Error("Redis mode attempted a platform restart")
		return true
	})
	if monitor.bindings != 0 || monitor.redial != nil || monitor.idleRestart != nil {
		t.Fatal("Redis mode acquired API recovery callbacks")
	}
}

func TestWorkPlatformRecoveryBindsWithoutExecutingAndRedialsEveryClient(t *testing.T) {
	isolateWorkPlatformRecovery(t)
	monitor := &workPlatformRecoveryFixture{}
	var order []string
	idleCalls := 0
	bindWorkPlatformRecovery(true, monitor, []func(){
		func() { order = append(order, "api") },
		func() { order = append(order, "auth") },
	}, func(func()) bool { idleCalls++; return false })
	if monitor.bindings != 1 || monitor.redial == nil || monitor.idleRestart == nil || len(order) != 0 || idleCalls != 0 {
		t.Fatal("binding was incomplete or performed recovery during startup")
	}
	monitor.redial()
	monitor.redial()
	if !reflect.DeepEqual(order, []string{"api", "auth", "api", "auth"}) || idleCalls != 0 {
		t.Fatalf("redial missed/reordered clients or invoked restart: order=%v idle_calls=%d", order, idleCalls)
	}
}

func TestWorkPlatformRecoveryServiceManagerAndIdleGuard(t *testing.T) {
	for _, test := range []struct {
		name          string
		invocation    string
		launchd       bool
		idle          bool
		wantIdleCalls int
		wantExitCalls int
	}{
		{name: "unmanaged", idle: true},
		{name: "whitespace is unmanaged", invocation: " \t\n", idle: true},
		{name: "launchd alone stays redial-only", launchd: true, idle: true},
		{name: "systemd idle accepts restart", invocation: " fixture-systemd-unit ", idle: true, wantIdleCalls: 1, wantExitCalls: 1},
		{name: "systemd busy or draining refuses restart", invocation: "fixture-systemd-unit", wantIdleCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateWorkPlatformRecovery(t)
			t.Setenv("INVOCATION_ID", test.invocation)
			if test.launchd {
				t.Setenv("LAUNCH_JOBKEY_LABEL", "fixture.citadel")
				t.Setenv("XPC_SERVICE_NAME", "fixture.citadel")
			}
			monitor := &workPlatformRecoveryFixture{}
			idleCalls, exitCalls, redialCalls := 0, 0, 0
			exit := func() { exitCalls++ }
			bindWorkPlatformRecovery(true, monitor, []func(){func() { redialCalls++ }}, func(received func()) bool {
				idleCalls++
				if !test.idle {
					return false
				}
				received()
				return true
			})
			got := monitor.idleRestart(exit)
			if got != (test.wantExitCalls != 0) || idleCalls != test.wantIdleCalls || exitCalls != test.wantExitCalls || redialCalls != 0 {
				t.Fatalf("guard/restart binding mismatch: accepted=%t idle=%d exit=%d redial=%d", got, idleCalls, exitCalls, redialCalls)
			}
			// Even an unmanaged process keeps its separately bounded redial path.
			monitor.redial()
			if redialCalls != 1 || idleCalls != test.wantIdleCalls || exitCalls != test.wantExitCalls {
				t.Fatal("redial depended on restart authority or re-executed a restart")
			}
		})
	}
}

func TestWorkPlatformRecoveryChecksServiceManagerAtRestartTime(t *testing.T) {
	isolateWorkPlatformRecovery(t)
	monitor := &workPlatformRecoveryFixture{}
	idleCalls, exitCalls := 0, 0
	bindWorkPlatformRecovery(true, monitor, nil, func(exit func()) bool { idleCalls++; exit(); return true })
	exit := func() { exitCalls++ }
	t.Setenv("INVOCATION_ID", "fixture-systemd-unit")
	if !monitor.idleRestart(exit) || idleCalls != 1 || exitCalls != 1 {
		t.Fatal("restart authority was captured before invocation")
	}
	t.Setenv("INVOCATION_ID", "")
	if monitor.idleRestart(exit) || idleCalls != 1 || exitCalls != 1 {
		t.Fatal("old restart authority survived a later guard refusal")
	}
}
