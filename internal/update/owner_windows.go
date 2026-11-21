//go:build windows

package update

import "golang.org/x/sys/windows"

// ExecutableOwnership reports the owner of the file at path and the current
// process token's user, as SID strings plus display names where resolvable.
// It reads the same owner the first-lock check compares.
func ExecutableOwnership(path string) (OwnerInfo, error) {
	id, err := windowsInstalledIdentity(path)
	if err != nil {
		return OwnerInfo{}, err
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return OwnerInfo{}, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return OwnerInfo{}, err
	}
	info := OwnerInfo{OwnerSID: id.owner.String(), UserSID: user.User.Sid.String()}
	info.OwnerName = accountName(id.owner)
	info.UserName = accountName(user.User.Sid)
	return info, nil
}

func accountName(sid *windows.SID) string {
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return sid.String()
	}
	return domain + `\` + account
}
