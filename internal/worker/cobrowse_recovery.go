package worker

// CobrowseJobRecoveryAvailable is derived from the actual configured handler,
// never a release version. A disabled, absent, or unbound adapter reports false.
func (r *Runner) CobrowseJobRecoveryAvailable() bool {
	for _, h := range r.handlers {
		a, ok := h.(*LegacyHandlerAdapter)
		if !ok || !a.CanHandle(JobTypeCobrowseSession) {
			continue
		}
		if protocol, ok := a.handler.(interface{ CobrowseJobRecoveryAvailable() bool }); ok && protocol.CobrowseJobRecoveryAvailable() {
			return true
		}
	}
	return false
}
