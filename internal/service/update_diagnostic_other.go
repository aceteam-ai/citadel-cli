//go:build !linux

package service

func DiagnoseWorkerUpdate(worker ManagedWorker) WorkerUpdateDiagnostic {
	return WorkerUpdateDiagnostic{Unit: worker.Unit.Description(), WorkerUser: worker.User, Warning: true, Detail: "managed worker install permissions are not inspected on this platform"}
}
