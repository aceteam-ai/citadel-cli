package event

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

var sshLogin = regexp.MustCompile(`^(Accepted|Failed) ([A-Za-z0-9_/-]+) for (invalid user )?([^ ]+) from ([^ ]+) port ([0-9]+)(?: ssh[12](?:: .*)?)?(?: \[preauth\])?$`)
var sshInvalidUser = regexp.MustCompile(`^Invalid user ([^ ]+) from ([^ ]+) port ([0-9]+)(?: \[preauth\])?$`)

func parseOpenSSH(line string, meta Envelope) (map[string]any, error) {
	fields := make(map[string]any)
	message := line
	if strings.HasPrefix(line, "<") || header3164.MatchString(line) {
		var err error
		fields, err = parseSyslog("syslog", line, meta)
		if err != nil {
			return nil, err
		}
		message, _ = fields["message"].(string)
	} else if parts := isoSyslogHeader.FindStringSubmatch(line); parts != nil {
		stamp, err := time.Parse(time.RFC3339Nano, parts[1])
		if err != nil {
			return nil, errors.New("invalid OpenSSH timestamp")
		}
		fields["@timestamp"] = stamp.UTC().Format(time.RFC3339Nano)
		fields["host.name"] = parts[2]
		if err := addProgramMessage(parts[3], fields); err != nil {
			return nil, err
		}
		message, _ = fields["message"].(string)
	} else if programTag.MatchString(line) {
		if err := addProgramMessage(line, fields); err != nil {
			return nil, err
		}
		message, _ = fields["message"].(string)
	}
	if process, exists := fields["process.name"]; exists && process != "sshd" && process != "sshd-session" {
		return nil, errors.New("OpenSSH event has an unexpected process name")
	}
	fields["message"] = message
	if _, exists := fields["process.name"]; !exists {
		fields["process.name"] = "sshd"
	}
	fields["event.category"] = "authentication"
	fields["event.action"] = "ssh_login"
	var user, address, port string
	if parts := sshLogin.FindStringSubmatch(message); parts != nil {
		fields["event.outcome"] = "failure"
		if parts[1] == "Accepted" {
			if parts[3] != "" {
				return nil, errors.New("accepted OpenSSH event cannot name an invalid user")
			}
			fields["event.outcome"] = "success"
		}
		fields["ssh.auth.method"] = parts[2]
		user, address, port = parts[4], parts[5], parts[6]
		if parts[3] != "" {
			fields["user.exists"] = false
		}
	} else if parts := sshInvalidUser.FindStringSubmatch(message); parts != nil {
		fields["event.outcome"] = "failure"
		fields["event.action"] = "ssh_invalid_user"
		fields["user.exists"] = false
		user, address, port = parts[1], parts[2], parts[3]
	} else {
		return nil, errors.New("unsupported or malformed OpenSSH auth event")
	}
	ip, err := parseIP(address)
	if err != nil {
		return nil, err
	}
	portNumber, err := parseInteger(port, 1, 65535)
	if err != nil {
		return nil, errors.New("invalid OpenSSH source port")
	}
	fields["user.name"] = user
	fields["source.ip"] = ip
	fields["source.port"] = portNumber
	return fields, nil
}
