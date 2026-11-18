package memorygit

import "github.com/aceteam-ai/citadel-cli/internal/botprofile"

// botprofile.DenyPath fails closed without a resolver; these tests protect no
// config dir. Production gets the real resolver from internal/jobs.
func init() { botprofile.ConfigDirs = func() []string { return nil } }
