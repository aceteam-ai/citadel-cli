package containerjobs

// Deliberate Docker-only exception to catalog.ContainerRuntime: quota image
// provisioning and the Engine exec completion protocol are not that interface.
// Never resolve privileged programs from the worker's inherited PATH.
func nativeProgramPath(name string) string {
	switch name {
	case "docker", "mount", "umount", "chown":
		return "/usr/bin/" + name
	case "mkfs.ext4":
		return "/usr/sbin/mkfs.ext4"
	default:
		return ""
	}
}
