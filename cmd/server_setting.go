package cmd

import (
	"errors"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const defaultControlPlaneServer = "https://aceteam.ai"

var serverURLFlag string
var errServerURL = errors.New("server must be an HTTPS origin or bare host (HTTP is allowed only on loopback)")
var errServerSwitch = errors.New("server differs from saved enrollment; follow the server reset procedure in docs/server-setting.md (logout alone does not clear enrollment or private-CA bindings)")
var serverHostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type serverSelection struct {
	Flag, LegacyFlag, Env, LegacyEnv, Saved string
	FlagSet, LegacyFlagSet                  bool
}

// resolveServerSelection is the single precedence/normalization rule. A saved
// enrollment cannot be redirected by an invocation override. Errors never echo
// an invalid URL, which may contain accidentally supplied credentials.
func resolveServerSelection(s serverSelection) (string, error) {
	saved := ""
	var err error
	if s.Saved != "" {
		saved, err = normalizeServerURL(s.Saved)
		if err != nil {
			return "", err
		}
	}
	value := saved
	switch {
	case s.FlagSet:
		value, err = normalizeServerURL(s.Flag)
		if err != nil {
			return "", err
		}
		if s.LegacyFlagSet {
			alias, err := normalizeServerURL(s.LegacyFlag)
			if err != nil {
				return "", err
			}
			if alias != value {
				return "", errors.New("--server and --auth-service disagree")
			}
		}
	case s.LegacyFlagSet:
		value, err = normalizeServerURL(s.LegacyFlag)
	case s.Env != "":
		value, err = normalizeServerURL(s.Env)
	case s.LegacyEnv != "":
		value, err = normalizeServerURL(s.LegacyEnv)
	}
	if err != nil {
		return "", err
	}
	if value == "" {
		value = defaultControlPlaneServer
	}
	if saved != "" && value != saved {
		return "", errServerSwitch
	}
	return value, nil
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\t\\?#") {
		return "", errServerURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errServerURL
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	if host == "" || len(host) > 253 {
		return "", errServerURL
	}
	if ip == nil {
		for _, label := range strings.Split(host, ".") {
			if !serverHostLabel.MatchString(label) {
				return "", errServerURL
			}
		}
	}
	if scheme != "https" && (scheme != "http" || !(host == "localhost" || ip != nil && ip.IsLoopback())) {
		return "", errServerURL
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", errServerURL
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errServerURL
		}
		port = strconv.Itoa(n)
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host, nil
}

func serverFlagValue(cmd *cobra.Command, name string) string {
	if cmd == nil {
		return ""
	}
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag.Value.String()
	}
	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		return flag.Value.String()
	}
	return ""
}

func resolveInvocationServer(cmd *cobra.Command, saved string) (string, error) {
	return resolveServerSelection(serverSelection{
		Flag: serverFlagValue(cmd, "server"), FlagSet: flagChanged(cmd, "server"),
		LegacyFlag: serverFlagValue(cmd, "auth-service"), LegacyFlagSet: flagChanged(cmd, "auth-service"),
		Env: os.Getenv("CITADEL_SERVER"), LegacyEnv: os.Getenv("CITADEL_AUTH_HOST"), Saved: saved,
	})
}
