//go:build windows

package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows pins an open directory against rename. Exercise that native boundary,
// and separately require a lost root handle to keep the runtime unacknowledged.
func TestWindowsSuspendRuntimePayloadAnchor(t *testing.T) {
	for _, lost := range []bool{false, true} {
		name := "rename-blocked"
		if lost {
			name = "handle-lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newSuspendRuntimeFixture(t)
			proof := f.s.proc.stopForSuspend
			rootPath := f.store.payloads.root.Name()
			var original []byte
			var payloadName string
			f.s.proc.stopForSuspend = func(ctx context.Context) error {
				if err := proof(ctx); err != nil {
					return err
				}
				if f.proofs != 2 {
					return nil
				}
				current, err := f.store.read(f.live.ID)
				if err != nil {
					t.Fatal(err)
				}
				payloadName, err = suspendManifestName(current)
				if err != nil {
					t.Fatal(err)
				}
				original, err = os.ReadFile(filepath.Join(rootPath, payloadName))
				if err != nil {
					t.Fatal(err)
				}
				if lost {
					if err := f.store.payloads.root.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(rootPath, rootPath+".retained"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
						t.Fatal("open payload directory did not reject rename with sharing violation", err)
					}
					if _, err := os.Lstat(rootPath + ".retained"); !os.IsNotExist(err) {
						t.Fatal("refused rename created a detached payload directory", err)
					}
				}
				return nil
			}
			err := f.run(t, nil)
			if lost {
				if err == nil || f.m.sessions[f.live.ID] != f.s || f.s.proc == nil {
					t.Fatal("lost payload handle acknowledged or released runtime", err)
				}
			} else if err != nil || len(f.m.sessions) != 0 || f.s.proc != nil {
				t.Fatal("OS-pinned payload prevented valid completion", err)
			}
			kept, readErr := os.ReadFile(filepath.Join(rootPath, payloadName))
			if readErr != nil || len(original) == 0 || !bytes.Equal(kept, original) {
				t.Fatal("payload handle check changed captured bytes", readErr)
			}
		})
	}
}

// The existing native Windows CI selects TestWindows*. Exercise the same
// storage/OS-lock contract there without changing or bypassing the workflow.
func TestWindowsSuspendManifestStorage(t *testing.T) {
	for name, test := range map[string]func(*testing.T){
		"runtime-complete":         TestSuspendRuntimeCaptureToDurableSlotRetirement,
		"runtime-failures":         TestSuspendRuntimeFailureRetainsSlotAndCapture,
		"runtime-admission":        TestSuspendRuntimeAdmissionBeforeEffects,
		"runtime-stop-race":        TestSuspendRuntimeStopWaitsUntilCommittedSlotRetirement,
		"runtime-cancellation":     TestSuspendRuntimeSharesBudgetAndRetainsUncertainEffects,
		"runtime-payload-anchor":   TestWindowsSuspendRuntimePayloadAnchor,
		"runtime-reconciliation":   TestSuspendRuntimeReconcilesUncertainPublishWithoutRefreshingCapture,
		"runtime-cleanup-retry":    TestSuspendRuntimeRetryAfterCleanupFailureKeepsOriginalCapture,
		"legacy-observer-fixture":  TestRestartFixtureJoinsObserverBeforeReset,
		"delete-retained-stop":     TestSuspendDeletePublicStopRetainsLifecycle,
		"delete-stop-all":          TestSuspendDeleteStopAllStillCleansOrdinarySessions,
		"delete-publication-wait":  TestSuspendDeleteRechecksAfterPublicationDuringWait,
		"delete-unknown-metadata":  TestSuspendDeleteUnknownMetadataIsNotDiscardAuthority,
		"delete-legacy-admission":  TestSuspendDeleteLegacyCompatibilityAndAdmission,
		"delete-budget-root":       TestSuspendDeleteWaitBudgetAndRootReplacement,
		"delete-long-budget":       TestSuspendDeleteLongWaitIsCapped,
		"legacy-cleanup-markers":   TestSuspendLegacyCleanupRetentionMarkers,
		"legacy-sweep-protection":  TestSuspendRecoveryProtectsRetainedResourcesFromLegacySweep,
		"legacy-sweep-local":       TestSuspendRecoveryLocalSweepDoesNotReadNodeRegistry,
		"start-retained-id":        TestSuspendStartPublicRefusesRetainedIdentityBeforeEffects,
		"start-compatibility":      TestSuspendStartLegacyAndLocalCompatibility,
		"start-fresh-public":       TestSuspendStartPublicFreshReservation,
		"start-startup-recheck":    TestSuspendStartRechecksAfterStartupSweep,
		"start-wait-recheck":       TestSuspendStartWaitRechecksPublishedState,
		"start-budget-admission":   TestSuspendStartAdmissionAndBudget,
		"scope-admission":          TestSuspendScopesAdmissionBeforeEffects,
		"scope-complete-proof":     TestSuspendScopesRequireBothExactProofsAndReapers,
		"scope-refusal-retry":      TestSuspendScopesFailuresAreRetryableAndOpaque,
		"scope-budget-exclusion":   TestSuspendScopesShareBudgetAndSerialize,
		"scope-graceful-compose":   TestSuspendScopesComposeWithGracefulResourceStop,
		"stop-complete-proof":      TestSuspendStopRequiresCompleteProofAfterGracefulClose,
		"stop-admission":           TestSuspendStopRefusesBeforeAnyEffect,
		"stop-fences":              TestSuspendStopFencesCloseAndProof,
		"stop-budget":              TestSuspendStopSharesOriginalBoundedBudget,
		"stop-capture-completion":  TestSuspendStopComposesWithDurableCaptureCompletion,
		"stage-capture-order":      TestSuspendStagedCaptureOrdersIntentBeforeContinuation,
		"stage-capture-input":      TestSuspendStagedCaptureRejectsInputsBeforePageIO,
		"stage-capture-failure":    TestSuspendStagedCaptureFailureRetainsReferencedIntent,
		"capture-use-lock":         TestSuspendCaptureContinuationRetainsActionExclusion,
		"capture-use-input":        TestSuspendCaptureContinuationRejectsBeforeIO,
		"capture-use-exit":         TestSuspendCaptureContinuationFailureAndCancellation,
		"capture-use-stage":        TestSuspendCaptureContinuationStagesWithoutUnlockGap,
		"maintenance-schedule":     TestSuspendMaintenanceScheduleBudgetAndBackoff,
		"maintenance-unused":       TestSuspendMaintenanceMissingStoreAndLocalMode,
		"maintenance-open-mode":    TestSuspendMaintenanceNonCreatingStoreOpen,
		"maintenance-batches":      TestSuspendMaintenanceBatchedExpiryAndOrphans,
		"maintenance-progress":     TestSuspendMaintenancePartialFailureRetainsCursor,
		"maintenance-in-flight":    TestSuspendMaintenanceRetainsTransitionalCaptures,
		"maintenance-exclusion":    TestSuspendMaintenanceOwnershipAndRegistryWaitBudget,
		"maintenance-cancellation": TestSuspendMaintenanceSingleSchedulerAndCancellation,
		"dormant-status-lifecycle": TestSuspendDormantStatusLifecycleProjection,
		"dormant-status-expiry":    TestSuspendDormantStatusExpiryIsReadOnly,
		"dormant-status-invalid":   TestSuspendDormantStatusRejectsInvalidMetadata,
		"dormant-status-manager":   TestSuspendDormantStatusManagerRestartAndLocalIsolation,
		"dormant-status-bounds":    TestSuspendDormantStatusReadBoundsAndMissingRegistry,
		"dormant-status-list-cap":  TestSuspendDormantStatusListResourceBounds,
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
		"restore-scroll-pinned":    TestSuspendScrollRestorePinnedIsolatedAttempt,
		"restore-scroll-budget":    TestSuspendScrollRestoreFencesAndBudget,
		"restore-scroll-documents": TestSuspendScrollRestoreRejectsUnreadyDocuments,
		"restore-scroll-script":    TestSuspendScrollRestoreScriptBehavior,
		"graceful-close-reaper":    TestSuspendGracefulCloseRequiresPinnedReaper,
		"graceful-close-admission": TestSuspendGracefulCloseAdmissionBeforeIO,
		"graceful-close-fences":    TestSuspendGracefulCloseFencesEachAwait,
		"graceful-close-providers": TestSuspendGracefulCloseProviderRefusals,
		"graceful-close-wait":      TestSuspendGracefulCloseWaitsWithinOneBudget,
		"graceful-close-cap":       TestSuspendGracefulCloseLongBudgetIsCapped,
		"graceful-close-cancel":    TestSuspendGracefulCloseCancellationAtEachAwait,
		"graceful-close-retry":     TestSuspendGracefulCloseReapedRetryStillFenced,
		"graceful-close-local":     TestSuspendGracefulCloseFenceCannotCancelAndAcknowledge,
		"restored-pages-complete":  TestSuspendRestoredPagesCompleteOrderedObservation,
		"page-scrolls-two-passes":  TestSuspendPageScrollsRestoreThenObserveEntireSet,
		"page-scrolls-snapshot":    TestSuspendPageScrollsCopiesPlanAcrossBothPasses,
		"page-scrolls-read-only":   TestSuspendPageScrollsFinalObservationDoesNotRepair,
		"page-scrolls-admission":   TestSuspendPageScrollsAdmissionBeforeEffects,
		"page-scrolls-failures":    TestSuspendPageScrollsFailureKeepsPartialEffectsUnacknowledged,
		"page-scrolls-budget":      TestSuspendPageScrollsSharesOriginalBudget,
		"restored-pages-copy":      TestSuspendRestoredPagesCopiesCallerPlan,
		"restored-pages-admission": TestSuspendRestoredPagesAdmissionBeforeIO,
		"restored-pages-budget":    TestSuspendRestoredPagesKeepsOriginalBudget,
		"restored-pages-window":    TestSuspendRestoredPagesAcceptsWindowBound,
		"restored-pages-active":    TestSuspendRestoredPagesActiveReadFailure,
		"restored-pages-discovery": TestSuspendRestoredPagesRefusesIncompleteDiscovery,
		"restored-pages-await":     TestSuspendRestoredPagesFencesEveryAwait,
		"restored-pages-cdp":       TestSuspendRestoredPagesReadOnlyCDPFences,
		"restored-pages-script":    TestSuspendRestoredPagesReadOnlyScript,
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
		"completion-order":         TestSuspendCompletionPersistsAfterVerifiedTeardown,
		"repeated-throwaway-cycle": TestSuspendRepeatedThrowawayCyclesAcrossStoreRestarts,
		"completion-replay":        TestSuspendCompletionFailureReplay,
		"completion-refusals":      TestSuspendCompletionRequiresCaptureAndIntent,
		"completion-visible":       TestSuspendCompletionVisibleRecordStillRequiresDurability,
		"completion-accounting":    TestSuspendCompletionRefusesUnaccountableRegistry,
		"completion-concurrency":   TestSuspendCompletionConcurrentRetryStopsOnlyOnce,
		"completion-clock-fence":   TestSuspendCompletionRejectsClockBeforeCapture,
		"completion-final-capture": TestSuspendCompletionRechecksCaptureAfterTeardown,
		"completion-receipt-time":  TestSuspendCompletionRejectsInvalidVisibleReceiptTime,
		"reopen-intent-order":      TestSuspendReopenIntentDurableSameIDAndCapture,
		"reopen-intent-replay":     TestSuspendReopenIntentFailureReplay,
		"reopen-intent-refusals":   TestSuspendReopenIntentRequiresCurrentPlanAndPayload,
		"reopen-intent-visible":    TestSuspendReopenIntentVisibleRecordStillNeedsDurability,
		"reopen-concurrency":       TestSuspendReopenIntentConcurrentRetryPublishesOnce,
		"reopened-live":            TestSuspendReopenedDurableVerifiedLive,
		"reopened-replay":          TestSuspendReopenedFailureAndVisibleReplay,
		"reopened-refusals":        TestSuspendReopenedAdmissionKeepsIntent,
		"reopened-await-fences":    TestSuspendReopenedFencesAwaitAndReplay,
		"reopened-after-rollback":  TestSuspendReopenedAfterRollbackKeepsAdvancedGeneration,
		"reopened-concurrency":     TestSuspendReopenedConcurrentVerificationPublishesOnce,
		"job-empty-proof":          TestSuspendJobRequiresWholeJobAndReaper,
		"job-refusal-retry":        TestSuspendJobRefusalsAndRetry,
		"job-legacy-no-proof":      TestSuspendJobLegacySuccessIsNotPositiveProof,
		"job-concurrent-stops":     TestSuspendJobWaitBudgetAndConcurrentStops,
		"job-failed-legacy":        TestSuspendJobFailureStillAllowsLegacyCleanup,
		"job-maximum-budget":       TestSuspendJobLongBudgetHasCeiling,
		"rollback-reopen-cycle":    TestSuspendRollbackInterruptedReopenThenReopen,
		"rollback-generation":      TestSuspendRollbackGenerationValidationStaysStrict,
		"rollback-refusals":        TestSuspendRollbackRefusalsKeepIntent,
		"rollback-replay":          TestSuspendRollbackFailureReplay,
		"rollback-reopen-clock":    TestSuspendRollbackReopenRejectsBackdatedCommit,
		"expiry-clock":             TestSuspendExpiryClockAndProfileRetention,
		"expiry-replay":            TestSuspendExpiryFailureReplay,
		"expiry-flush-retry":       TestSuspendExpiryReconcilesVisibleTombstone,
		"expiry-sibling":           TestSuspendExpiryKeepsSiblingPayload,
		"expiry-refusals":          TestSuspendExpiryRefusalsLeavePayload,
		"tombstone-retention":      TestSuspendTombstoneRetentionAndIdempotentPrune,
		"tombstone-failures":       TestSuspendTombstoneOtherReasonsAndDeleteFailures,
		"ttl-batch-budget":         TestSuspendTTLSweepBoundedLevelTriggeredPasses,
		"ttl-batch-progress":       TestSuspendTTLSweepFailedEntryDoesNotStarveSiblings,
		"ttl-batch-empty":          TestSuspendTTLSweepBudgetAndEmptyRegistry,
		"ttl-batch-flush-retry":    TestSuspendTTLSweepFlushesUncertainMetadataOnNextPass,
		"ttl-batch-earned-cursor":  TestSuspendTTLSweepKeepsEarnedCursor,
		"ttl-recovery-retention":   TestSuspendRecoveryLeavesTombstonesToDurableSweep,
		"ttl-ended-lookup":         TestSuspendExpiredLookupKeepsSevenDayReason,
		"recovery-pass-bounds":     TestSuspendRecoveryPassBoundedRestartAndRetry,
		"recovery-pass-progress":   TestSuspendRecoveryPassFailuresDoNotStarveSiblings,
		"recovery-pass-refusals":   TestSuspendRecoveryPassAdmissionAndOtherStates,
		"recovery-pass-cancel":     TestSuspendRecoveryPassCancellationPreservesCursor,
		"recovery-pass-visible":    TestSuspendRecoveryPassReconcilesVisibleMetadata,
		"recovery-pass-payload":    TestSuspendRecoveryPassMissingCaptureRequiresVerifiedTeardown,
		"failure-stop-proof":       TestSuspendFailureRequiresVerifiedTeardown,
		"failure-refusals":         TestSuspendFailureRefusalsLeaveIntent,
		"failure-commit-retry":     TestSuspendFailureCommitReplay,
		"failure-reappeared":       TestSuspendFailureReappearingCaptureRemainsRecoverable,
		"failure-visible-refusal":  TestSuspendFailureVisibleReceiptRefusals,
		"failure-absence-recheck":  TestSuspendFailureAbsenceRecheckedBeforeStop,
		"failure-profile-keep":     TestSuspendFailureRetainsSavedProfileDescriptor,
		"recovery-pass-scan-error": TestSuspendRecoveryPassKeepsCursorOnInitialFailure,
		"orphan-pass-bounds":       TestSuspendOrphansBoundedProgressAndDurability,
		"orphan-retained-states":   TestSuspendOrphansPreserveAllReferencedStates,
		"orphan-expiry-cycle":      TestSuspendOrphansAfterExpiry,
		"orphan-bad-tombstone":     TestSuspendOrphansRefuseMalformedTombstones,
		"writer-stop-race":         TestSuspendWriterDoesNotResurrectStoppedRecord,
		"writer-incarnation-race":  TestSuspendWriterDoesNotOverwriteNewLifecycle,
		"writer-publication-lock":  TestSuspendWriterPublicationSharesLifecycleLock,
		"writer-current-metadata":  TestSuspendWriterSnapshotsMetadataAfterIdentity,
		"writer-refusals":          TestSuspendWriterNonLiveAdmissionAndWriteFailure,
		"registry-gate-writers":    TestSuspendRegistryGateLegacyExclusion,
		"registry-gate-budget":     TestSuspendRegistryGateBudgetAndRelease,
		"registry-gate-identity":   TestSuspendRegistryGateIdentityAndBound,
		"registry-gate-replace":    TestSuspendRegistryGateRejectsReplacedDirectory,
		"registry-gate-anchored":   TestSuspendRegistryGateAnchoredCompletion,
		"snapshot-prune":           TestSuspendRegistrySnapshotPrunePreservesNewerRecord,
		"snapshot-cas":             TestSuspendRegistrySnapshotMutation,
		"snapshot-race":            TestSuspendRegistrySnapshotRecoveryPreservesNewerRecord,
		"snapshot-progress":        TestSuspendRegistrySnapshotRecoveryAndRetentionStillProgress,
		"orphan-refused-scan":      TestSuspendOrphansRefuseUncertainScanBeforeDeletion,
		"orphan-failure-retry":     TestSuspendOrphansFailuresDoNotStarveAndRetryFlushes,
		"orphan-await-recheck":     TestSuspendOrphansRecheckReferencesAndFileIdentity,
		"orphan-canonical-names":   TestSuspendOrphansCanonicalNames,
		"orphan-filesystem":        TestSuspendOrphansFilesystemRefusals,
	} {
		t.Run(name, test)
	}
}
