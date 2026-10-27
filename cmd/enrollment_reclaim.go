package cmd

// A fresh device grant cannot delete a predecessor's registration. Leave that
// replacement decision to platform approval, without loading an old credential.
// Normal grant flows use their in-memory token even if persistence failed;
// only legacy authkey flows without a current grant may read saved credentials.
func reclaimEnrollmentNodes(forceNew bool, current *DeviceAuthResult, hostnames ...string) {
	reclaimEnrollmentNodesWith(forceNew, current, getDeviceConfigFromFile, reclaimStaleNodeByHostname, hostnames...)
}

func reclaimEnrollmentNodesWith(forceNew bool, current *DeviceAuthResult,
	loadSaved func() *DeviceConfig, reclaim func(string, string), hostnames ...string) {
	if forceNew {
		return
	}
	token := ""
	if current != nil {
		if current.Token != nil {
			token = current.Token.DeviceAPIToken
		}
	} else if loadSaved != nil {
		if saved := loadSaved(); saved != nil {
			token = saved.DeviceAPIToken
		}
	}
	if token == "" || reclaim == nil {
		return
	}
	seen := make(map[string]bool)
	for _, hostname := range hostnames {
		if hostname != "" && !seen[hostname] {
			seen[hostname] = true
			reclaim(token, hostname)
		}
	}
}
