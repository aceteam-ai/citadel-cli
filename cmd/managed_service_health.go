package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
)

// managedServiceHealth is the shared status/doctor view of one manifest
// service. OK means the observed state matches desired_status: services default
// to running, while an explicit desired_status: stopped expects no live
// container or native process.
type managedServiceHealth struct {
	Name   string
	State  string
	Detail string
	OK     bool
	Native bool
}

type managedServiceStateProbe func(composePath, serviceName string) (compose.ServiceState, error)

// checkManagedServiceHealth keeps the two operator surfaces in agreement. The
// injected probe makes the state policy independently testable without a live
// Docker or Podman daemon.
func checkManagedServiceHealth(manifest *CitadelManifest, configDir string, probe managedServiceStateProbe) []managedServiceHealth {
	if manifest == nil {
		return nil
	}
	checks := make([]managedServiceHealth, 0, len(manifest.Services))
	for _, service := range manifest.Services {
		check := managedServiceHealth{Name: service.Name}
		composePath := ""
		if service.ComposeFile == "" {
			if !strings.EqualFold(strings.TrimSpace(service.Type), "native") {
				check.State = "missing"
				check.Detail = "compose_file is not configured"
				checks = append(checks, check)
				continue
			}
		} else {
			composePath = filepath.Join(configDir, service.ComposeFile)
			if _, err := os.Stat(composePath); err != nil {
				check.State = "missing"
				if os.IsNotExist(err) {
					check.Detail = fmt.Sprintf("compose file not found: %s", service.ComposeFile)
				} else {
					check.Detail = fmt.Sprintf("cannot access compose file %s: %v", service.ComposeFile, err)
				}
				checks = append(checks, check)
				continue
			}
		}

		state, err := probe(composePath, service.Name)
		if err != nil {
			check.State = "error"
			check.Detail = err.Error()
			checks = append(checks, check)
			continue
		}
		check.Native = state.Native
		if serviceStartDisabled(service) {
			switch {
			case state.Running:
				check.State = compose.StateRunning
				check.Detail = "service is running despite desired_status: stopped"
			case state.AnyRunning:
				check.State = compose.StatePartial
				check.Detail = "some service components are running despite desired_status: stopped"
			default:
				check.State = compose.StateStopped
				check.Detail = "desired_status: stopped"
				check.OK = true
			}
			checks = append(checks, check)
			continue
		}

		if state.Running {
			check.State = compose.StateRunning
			check.OK = true
			checks = append(checks, check)
			continue
		}
		if state.AnyRunning {
			check.State = compose.StatePartial
			check.Detail = "configured to run, but only some service components are running"
			checks = append(checks, check)
			continue
		}

		if state.Container == nil {
			check.State = "missing"
			check.Detail = "configured to run, but no container or native process is running"
		} else {
			check.State = strings.ToLower(strings.TrimSpace(state.Container.State))
			if check.State == "" {
				check.State = compose.StateStopped
			}
			check.Detail = "configured to run, but the service is not running"
		}
		checks = append(checks, check)
	}
	return checks
}

func probeManagedServiceState(composePath, serviceName string) (compose.ServiceState, error) {
	if composePath == "" {
		if internalServices.IsNativeServiceServing(serviceName) {
			return compose.ServiceState{State: compose.StateRunning, Running: true, Native: true}, nil
		}
		return compose.ServiceState{State: compose.StateStopped}, nil
	}
	psArgs := append(composeFileArgs(composePath, composePath), "ps", "--format", "json")
	output, err := composeCommand(psArgs...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return compose.ServiceState{}, fmt.Errorf("could not get service status: %s", detail)
	}
	return composeServiceStateFrom(output, composePath, serviceName), nil
}

func currentManagedServiceHealth() (checks []managedServiceHealth, checked bool) {
	manifest, configDir, err := findAndReadManifest()
	if err != nil || manifest == nil {
		return nil, false
	}
	return checkManagedServiceHealth(manifest, configDir, probeManagedServiceState), true
}

func managedServicesHealthy(checks []managedServiceHealth) bool {
	for _, check := range checks {
		if !check.OK {
			return false
		}
	}
	return true
}
