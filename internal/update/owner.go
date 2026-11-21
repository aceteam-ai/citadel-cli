package update

// OwnerInfo is the installed executable's owner next to the current user.
// Self-update refuses to provision its first lock unless they are the same
// account (see windowsInstalledIdentity / openWindowsMutationLock).
type OwnerInfo struct {
	OwnerSID  string
	OwnerName string
	UserSID   string
	UserName  string
}

// Matches reports whether the current user owns the executable.
func (o OwnerInfo) Matches() bool { return o.OwnerSID != "" && o.OwnerSID == o.UserSID }
