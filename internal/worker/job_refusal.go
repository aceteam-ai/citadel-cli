package worker

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// A refusal source explains a registration gate without advertising or running
// the withheld job. The runner consults it only after finding no runnable handler.
type jobRefusalSource interface {
	RefusalFor(string) *jobs.ShellRefusal
}

type registrationRefusals struct {
	opts LegacyHandlerOpts
	goos string
}

func (*registrationRefusals) CanHandle(string) bool { return false }
func (*registrationRefusals) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	err := &jobs.ShellRefusal{Reason: "handler_unavailable", Message: "registration refusal evidence cannot execute jobs"}
	return &JobResult{Status: JobStatusFailure, Error: err}, err
}

func permissionEnabled(live func() bool, disabled bool) bool {
	if live != nil {
		return live()
	}
	return !disabled
}

func legacyPermissionRefusal(jobType string) *jobs.ShellRefusal {
	switch jobType {
	case JobTypeShellCommand:
		return jobs.WorkflowShellRefusal(jobs.ReasonShellDisabled)
	case JobTypeFileScreenshot, JobTypeVNCScreenshot, JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions:
		return &jobs.ShellRefusal{Reason: "desktop_disabled", Message: "desktop permission is disabled on this node; enable Desktop in the AceTeam control center"}
	case JobTypeFileRead, JobTypeFileReadBytes, JobTypeFileReadRangeV1, JobTypeFileReadRangeV2, JobTypeFileWrite, JobTypeFileEdit, JobTypeFileList, JobTypeFileSearch, JobTypeFileIndex, JobTypeFileSemanticSearch, JobTypeFileFetch:
		return &jobs.ShellRefusal{Reason: "files_disabled", Message: "files permission is disabled on this node; enable Files in the AceTeam control center"}
	case JobTypeHuddleJoin, JobTypeMeetingJoin:
		return &jobs.ShellRefusal{Reason: "meeting_disabled", Message: "meeting capability is disabled on this node; enable it before dispatching new joins"}
	default:
		return &jobs.ShellRefusal{Reason: "capability_unavailable", Message: "this job's live node capability gate is closed"}
	}
}

func (g *registrationRefusals) RefusalFor(jobType string) *jobs.ShellRefusal {
	o := g.opts
	switch jobType {
	case JobTypeShellCommand:
		if g.goos == "windows" {
			return &jobs.ShellRefusal{Reason: "platform_unsupported", Message: "SHELL_COMMAND requires a Unix shell and is unavailable on native Windows; use a supported Unix node"}
		}
		if !permissionEnabled(o.ShellEnabled, o.ShellDisabled) {
			return legacyPermissionRefusal(jobType)
		}
	case JobTypeFileScreenshot, JobTypeVNCScreenshot, JobTypeVNCType, JobTypeVNCKeys, JobTypeVNCActions:
		if !permissionEnabled(o.DesktopEnabled, o.DesktopDisabled) {
			return legacyPermissionRefusal(jobType)
		}
	case JobTypeFileRead, JobTypeFileReadBytes, JobTypeFileReadRangeV1, JobTypeFileReadRangeV2, JobTypeFileWrite, JobTypeFileWriteBytes, JobTypeFileEdit, JobTypeFileList, JobTypeFileSearch, JobTypeFileIndex, JobTypeFileSemanticSearch, JobTypeFileFetch, JobTypeMeetingJoin:
		if o.WorkspaceDir == "" {
			return &jobs.ShellRefusal{Reason: "workspace_not_configured", Message: "this node has no workspace directory configured; configure a workspace before dispatching this job"}
		}
		if jobType == JobTypeMeetingJoin {
			if o.MeetingEnabled != nil && !o.MeetingEnabled() {
				return legacyPermissionRefusal(jobType)
			}
		} else if !permissionEnabled(o.FilesEnabled, o.FilesDisabled) {
			return legacyPermissionRefusal(jobType)
		} else if jobType == JobTypeFileFetch {
			return &jobs.ShellRefusal{Reason: "capability_unavailable", Message: gatedJobTypeReasons[jobType]}
		} else if jobType == JobTypeFileReadRangeV2 && g.goos != "linux" {
			return &jobs.ShellRefusal{Reason: "platform_unsupported", Message: "conditional file ranges require Linux artifact observations; use a supported Linux node"}
		}
	case JobTypeHuddleJoin:
		if o.MeetingEnabled != nil && !o.MeetingEnabled() {
			return legacyPermissionRefusal(jobType)
		}
	case JobTypeTranscribeAudio:
		if o.WorkspaceDir == "" {
			return &jobs.ShellRefusal{Reason: "workspace_not_configured", Message: "audio transcription requires a configured workspace directory"}
		}
	case JobTypeServiceStart, JobTypeServiceStop, JobTypeServiceStatus:
		if o.ConfigDir == "" {
			return &jobs.ShellRefusal{Reason: "config_not_configured", Message: "this node has no citadel.yaml manifest / config directory configured"}
		}
	}
	return nil
}

func (r *Runner) unavailableJobRefusal(jobType string) *jobs.ShellRefusal {
	for _, handler := range r.handlers {
		if source, ok := handler.(jobRefusalSource); ok {
			if refusal := source.RefusalFor(jobType); refusal != nil {
				return refusal
			}
		}
	}
	if message, ok := gatedJobTypeReasons[jobType]; ok {
		return &jobs.ShellRefusal{Reason: "capability_unavailable", Message: message}
	}
	for _, known := range allKnownJobTypes {
		if known == jobType {
			return &jobs.ShellRefusal{Reason: "handler_unavailable", Message: "this job type is known to this binary but no runnable handler is registered; check node configuration and advertised capabilities"}
		}
	}
	return nil
}
