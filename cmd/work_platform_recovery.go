package cmd

// workPlatformRecoveryMonitor is the callback boundary used by runWork after
// the real runner and its platform clients are ready. Keeping this binding in
// one helper lets fixtures test it without starting a worker or probing nodes.
type workPlatformRecoveryMonitor interface {
	SetPlatformRecovery(redial func(), idleRestart func(func()) bool)
}

func bindWorkPlatformRecovery(apiMode bool, monitor workPlatformRecoveryMonitor, redials []func(), tryIdleRestart func(func()) bool) {
	if !apiMode {
		return
	}
	monitor.SetPlatformRecovery(func() {
		for _, redial := range redials {
			redial()
		}
	}, func(exit func()) bool {
		// Match remote restart: only a service manager can bring us back.
		// The runner remains responsible for the atomic idle/admission fence.
		return managedByServiceManager() && tryIdleRestart(exit)
	})
}
