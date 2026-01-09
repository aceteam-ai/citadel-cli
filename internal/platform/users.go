package platform

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// UserManager interface defines operations for system user management
type UserManager interface {
	CreateUser(username string, system bool) error
	UserExists(username string) bool
	CreateGroup(groupname string, system bool) error
	GroupExists(groupname string) bool
	AddUserToGroup(username, groupname string) error
	IsUserInGroup(username, groupname string) bool
}

// GetUserManager returns the appropriate user manager for the current OS
func GetUserManager() (UserManager, error) {
	switch OS() {
	case "linux":
		return &LinuxUserManager{}, nil
	case "darwin":
		return &DarwinUserManager{}, nil
	default:
		return nil, fmt.Errorf("unsupported operating system: %s", OS())
	}
}

// LinuxUserManager implements UserManager for Linux systems
type LinuxUserManager struct{}

func (l *LinuxUserManager) UserExists(username string) bool {
	_, err := user.Lookup(username)
	return err == nil
}

func (l *LinuxUserManager) CreateUser(username string, system bool) error {
	if l.UserExists(username) {
		return nil // Already exists
	}

	args := []string{"--system", username}
	if !system {
		args = []string{username}
	}

	cmd := exec.Command("useradd", args...)
	return cmd.Run()
}

func (l *LinuxUserManager) GroupExists(groupname string) bool {
	_, err := user.LookupGroup(groupname)
	return err == nil
}

func (l *LinuxUserManager) CreateGroup(groupname string, system bool) error {
	if l.GroupExists(groupname) {
		return nil // Already exists
	}

	args := []string{}
	if system {
		args = append(args, "--system")
	}
	args = append(args, groupname)

	cmd := exec.Command("groupadd", args...)
	return cmd.Run()
}

func (l *LinuxUserManager) AddUserToGroup(username, groupname string) error {
	cmd := exec.Command("usermod", "-aG", groupname, username)
	return cmd.Run()
}

func (l *LinuxUserManager) IsUserInGroup(username, groupname string) bool {
	cmd := exec.Command("id", "-nG", username)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	groups := strings.Fields(string(output))
	for _, g := range groups {
		if g == groupname {
			return true
		}
	}
	return false
}

// DarwinUserManager implements UserManager for macOS systems
type DarwinUserManager struct{}

func (d *DarwinUserManager) UserExists(username string) bool {
	_, err := user.Lookup(username)
	return err == nil
}

func (d *DarwinUserManager) CreateUser(username string, system bool) error {
	if d.UserExists(username) {
		return nil // Already exists
	}

	// On macOS, we need to find an available UID
	uid, err := d.findAvailableUID(system)
	if err != nil {
		return fmt.Errorf("failed to find available UID: %w", err)
	}

	// Create the user using dscl
	commands := [][]string{
		{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username)},
		{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username), "UserShell", "/bin/bash"},
		{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username), "RealName", username},
		{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username), "UniqueID", strconv.Itoa(uid)},
		{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username), "PrimaryGroupID", "20"}, // staff group
	}

	// If not a system user, create a home directory
	if !system {
		homeDir := fmt.Sprintf("/Users/%s", username)
		commands = append(commands,
			[]string{"dscl", ".", "-create", fmt.Sprintf("/Users/%s", username), "NFSHomeDirectory", homeDir},
			[]string{"createhomedir", "-c", "-u", username},
		)
	}

	for _, cmdArgs := range commands {
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create user %s: %w", username, err)
		}
	}

	return nil
}

func (d *DarwinUserManager) findAvailableUID(system bool) (int, error) {
	// System users: 200-499
	// Regular users: 501+
	startUID := 501
	if system {
		startUID = 200
	}

	for uid := startUID; uid < startUID+300; uid++ {
		cmd := exec.Command("dscl", ".", "-list", "/Users", "UniqueID")
		output, err := cmd.Output()
		if err != nil {
			return 0, err
		}

		uidStr := strconv.Itoa(uid)
		if !strings.Contains(string(output), uidStr) {
			return uid, nil
		}
	}

	return 0, fmt.Errorf("no available UID found")
}

func (d *DarwinUserManager) GroupExists(groupname string) bool {
	_, err := user.LookupGroup(groupname)
	return err == nil
}

func (d *DarwinUserManager) CreateGroup(groupname string, system bool) error {
	if d.GroupExists(groupname) {
		return nil // Already exists
	}

	// Find an available GID
	gid, err := d.findAvailableGID(system)
	if err != nil {
		return fmt.Errorf("failed to find available GID: %w", err)
	}

	// Create the group using dscl
	commands := [][]string{
		{"dscl", ".", "-create", fmt.Sprintf("/Groups/%s", groupname)},
		{"dscl", ".", "-create", fmt.Sprintf("/Groups/%s", groupname), "PrimaryGroupID", strconv.Itoa(gid)},
	}

	for _, cmdArgs := range commands {
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create group %s: %w", groupname, err)
		}
	}

	return nil
}

func (d *DarwinUserManager) findAvailableGID(system bool) (int, error) {
	startGID := 501
	if system {
		startGID = 200
	}

	for gid := startGID; gid < startGID+300; gid++ {
		cmd := exec.Command("dscl", ".", "-list", "/Groups", "PrimaryGroupID")
		output, err := cmd.Output()
		if err != nil {
			return 0, err
		}

		gidStr := strconv.Itoa(gid)
		if !strings.Contains(string(output), gidStr) {
			return gid, nil
		}
	}

	return 0, fmt.Errorf("no available GID found")
}

func (d *DarwinUserManager) AddUserToGroup(username, groupname string) error {
	cmd := exec.Command("dscl", ".", "-append", fmt.Sprintf("/Groups/%s", groupname), "GroupMembership", username)
	return cmd.Run()
}

func (d *DarwinUserManager) IsUserInGroup(username, groupname string) bool {
	cmd := exec.Command("dscl", ".", "-read", fmt.Sprintf("/Groups/%s", groupname), "GroupMembership")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	return strings.Contains(string(output), username)
}
