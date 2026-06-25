package source

import (
	"encoding/json"
	"fmt"
)

// parseFileCredential decodes one on-disk credential file into a Credential.
//
// SECURITY: every error path here is written to describe WHAT is wrong without
// echoing the token value, so a parse failure can never leak the secret into a
// log or job payload.
func parseFileCredential(host string, data []byte) (Credential, error) {
	var fc fileCredential
	if err := json.Unmarshal(data, &fc); err != nil {
		// json.Unmarshal errors reference offsets/types, never field values, but
		// wrap with a fixed message to be certain no value is surfaced.
		return NoCredential, fmt.Errorf("credential for host %q is not valid JSON", host)
	}

	credHost := fc.Host
	if credHost == "" {
		credHost = host
	}

	switch fc.Type {
	case "https-token":
		if fc.Token == "" {
			return NoCredential, fmt.Errorf("credential for host %q (https-token) has an empty token", host)
		}
		user := fc.Username
		if user == "" {
			user = defaultHTTPSUser
		}
		return Credential{
			Kind:     CredHTTPSToken,
			Host:     credHost,
			Username: user,
			token:    fc.Token,
		}, nil
	case "ssh-key":
		if fc.KeyPath == "" {
			return NoCredential, fmt.Errorf("credential for host %q (ssh-key) has an empty key_path", host)
		}
		return Credential{
			Kind:    CredSSHKey,
			Host:    credHost,
			KeyPath: fc.KeyPath,
		}, nil
	case "":
		return NoCredential, fmt.Errorf("credential for host %q is missing its \"type\" field", host)
	default:
		return NoCredential, fmt.Errorf("credential for host %q has unknown type %q (want \"https-token\" or \"ssh-key\")", host, fc.Type)
	}
}
