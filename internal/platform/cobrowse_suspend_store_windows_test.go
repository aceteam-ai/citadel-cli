//go:build windows

package platform

import "testing"

// The existing native Windows CI selects TestWindows*. Exercise the same
// storage/OS-lock contract there without changing or bypassing the workflow.
func TestWindowsSuspendManifestStorage(t *testing.T) {
	for name, test := range map[string]func(*testing.T){
		"private-roundtrip":        TestSuspendStorePrivateRoundTripAndCreateOnly,
		"persistent-unlock":        TestSuspendStorePersistentUnlockAndCaptureGeneration,
		"real-profile-crypto":      TestSuspendStoreRealProfileCryptoRoundTrip,
		"capture-fence":            TestSuspendStoreRefusesMismatchedCapture,
		"persistence-failures":     TestSuspendStoreSyncBeforePublicationAndFailureCleanup,
		"filesystem-objects":       TestSuspendStoreRejectsUnsafeFilesystemObjects,
		"read-bounds":              TestSuspendStoreReadBoundsAndMetadata,
		"orphan-budget":            TestSuspendStoreCountsOrphansAndConcurrentWrites,
		"read-open":                TestSuspendStoreReadOnlyOpenAndPrivacy,
		"exclusive-owner":          TestSuspendStoreExclusiveAcrossInstances,
		"process-crash-lock":       TestSuspendStoreCrossProcessLockAndCrashRelease,
		"directory-failure":        TestSuspendStoreDirectorySyncFailureRemainsAccounted,
		"entry-cap":                TestSuspendStoreBoundsDirectoryEnumeration,
		"capture-isolation":        TestSuspendCaptureIsolatedBoundedRead,
		"capture-await-fence":      TestSuspendCaptureFencesEveryAwait,
		"capture-provider-failure": TestSuspendCaptureOpaqueProviderRefusals,
		"capture-values":           TestSuspendCaptureRejectsMalformedValues,
		"capture-deadline":         TestSuspendCaptureRequiresDeadlineBeforeConnecting,
		"capture-all-pages":        TestSuspendPagesCompletePinnedCapture,
		"capture-admission":        TestSuspendPagesAdmissionBeforeIO,
		"capture-page-fences":      TestSuspendPagesFencesEachProviderAwait,
		"capture-page-population":  TestSuspendPagesBoundsAndChangedPopulation,
		"intent-durability":        TestSuspendIntentPayloadBeforeDurableIntent,
		"intent-authority":         TestSuspendIntentAdmissionAndFinalAuthority,
		"intent-failures":          TestSuspendIntentPersistenceFailuresRetainAccounting,
		"intent-cancelled-commit":  TestSuspendIntentAuthorityLostAfterPublicationIsUnacknowledged,
		"intent-saved-profile":     TestSuspendIntentSavedProfileAndFakeClock,
		"intent-bounds":            TestSuspendIntentRegistryByteBounds,
		"intent-write-capacity":    TestSuspendIntentReservesAtomicReplacementCapacity,
		"intent-accounting":        TestSuspendIntentCapacityAndUnparseableRecords,
		"intent-concurrency":       TestSuspendIntentSerializesCompetingCaptures,
	} {
		t.Run(name, test)
	}
}
