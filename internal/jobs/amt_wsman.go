package jobs

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	amtNS      = "http://intel.com/wbem/wscim/1/amt-schema/1/"
	cimNS      = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/"
	wsmanNS    = "http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd"
	transferNS = "http://schemas.xmlsoap.org/ws/2004/09/transfer/"
)

var digestField = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)
var biosSetupField = regexp.MustCompile(`<(\w+:)?BIOSSetup>([^<]*)</(\w+:)?BIOSSetup>`)
var bootRoot = regexp.MustCompile(`<(\w+):AMT_BootSettingData(?:\s[^>]*)?>`)

func amtLANAddress(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || ip.String() != host {
		return false
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return true
		}
	}
	return false
}

type amtWSMan struct {
	host, username, password string
	client                   *http.Client
}

func newAMTWSMan(host, username, password string) (*amtWSMan, error) {
	if !amtLANAddress(host) || username == "" || password == "" || strings.ContainsAny(username, "\"\r\n") {
		return nil, errors.New("invalid AMT binding")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	return &amtWSMan{host: host, username: username, password: password, client: &http.Client{
		Transport: transport, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func md5Hex(data string) string {
	sum := md5.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

func digestAuthorization(challenge, username, password string) (string, error) {
	if !strings.HasPrefix(challenge, "Digest ") {
		return "", errors.New("AMT did not offer Digest authentication")
	}
	fields := map[string]string{}
	for _, match := range digestField.FindAllStringSubmatch(challenge, -1) {
		fields[strings.ToLower(match[1])] = match[2]
	}
	realm, nonce := fields["realm"], fields["nonce"]
	if realm == "" || nonce == "" || (fields["algorithm"] != "" && !strings.EqualFold(fields["algorithm"], "MD5")) {
		return "", errors.New("unsupported AMT Digest challenge")
	}
	qop := fields["qop"]
	if qop != "" && !strings.Contains(","+qop+",", ",auth,") && qop != "auth" {
		return "", errors.New("AMT Digest auth qop unavailable")
	}
	cbytes := make([]byte, 16)
	if _, err := rand.Read(cbytes); err != nil {
		return "", errors.New("AMT authentication unavailable")
	}
	cnonce := hex.EncodeToString(cbytes)
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex("POST:/wsman")
	response := md5Hex(ha1 + ":" + nonce + ":00000001:" + cnonce + ":auth:" + ha2)
	return fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri="/wsman", algorithm=MD5, qop=auth, nc=00000001, cnonce=%q, response=%q`, username, realm, nonce, cnonce, response), nil
}

func (a *amtWSMan) soap(ctx context.Context, action, uri, body, selectors string) ([]byte, error) {
	envelope := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" ` +
		`xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" ` +
		`xmlns:w="` + wsmanNS + `"><s:Header>` +
		`<a:Action>` + action + `</a:Action><a:To>/wsman</a:To>` +
		`<w:ResourceURI>` + uri + `</w:ResourceURI>` +
		`<a:MessageID>uuid:` + randomUUID() + `</a:MessageID>` +
		`<a:ReplyTo><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address></a:ReplyTo>` +
		selectors + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	endpoint := "http://" + net.JoinHostPort(a.host, "16992") + "/wsman"
	request := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(envelope))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/soap+xml;charset=UTF-8")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return a.client.Do(req)
	}
	first, err := request("")
	if err != nil {
		return nil, errors.New("AMT WS-MAN connection failed")
	}
	io.Copy(io.Discard, io.LimitReader(first.Body, 4096))
	first.Body.Close()
	if first.StatusCode != http.StatusUnauthorized {
		return nil, fmt.Errorf("AMT authentication challenge unavailable (%d)", first.StatusCode)
	}
	auth, err := digestAuthorization(first.Header.Get("WWW-Authenticate"), a.username, a.password)
	if err != nil {
		return nil, err
	}
	resp, err := request(auth)
	if err != nil {
		return nil, errors.New("AMT WS-MAN connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AMT WS-MAN refused operation (%d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("AMT WS-MAN response could not be read")
	}
	if bytes.Contains(raw, []byte("<s:Fault")) || bytes.Contains(raw, []byte("<Fault")) {
		return nil, errors.New("AMT WS-MAN returned a fault")
	}
	return raw, nil
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func xmlField(raw []byte, name string) string {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == name {
			var value string
			if decoder.DecodeElement(&value, &start) == nil {
				return strings.TrimSpace(value)
			}
			return ""
		}
	}
}

func (a *amtWSMan) get(ctx context.Context, uri string) ([]byte, error) {
	return a.soap(ctx, transferNS+"Get", uri, "", "")
}

func (a *amtWSMan) status(ctx context.Context) (map[string]any, error) {
	setup, err := a.get(ctx, amtNS+"AMT_SetupAndConfigurationService")
	if err != nil {
		return nil, err
	}
	redirection, err := a.get(ctx, amtNS+"AMT_RedirectionService")
	if err != nil {
		return nil, err
	}
	power, err := a.get(ctx, cimNS+"CIM_AssociatedPowerManagementService")
	if err != nil {
		return nil, err
	}
	provisioning := map[string]string{"0": "pre", "1": "in", "2": "post"}[xmlField(setup, "ProvisioningState")]
	mode := map[string]string{"1": "ACM", "4": "CCM"}[xmlField(setup, "ProvisioningMode")]
	state := map[string]string{"2": "on", "8": "off"}[xmlField(power, "PowerState")]
	if provisioning == "" {
		provisioning = "unknown"
	}
	if mode == "" {
		mode = "unknown"
	}
	if state == "" {
		state = "unknown"
	}
	return map[string]any{"provisioning": provisioning, "control_mode": mode, "power_state": state,
		"redirection_listener": xmlField(redirection, "ListenerEnabled")}, nil
}

func (a *amtWSMan) power(ctx context.Context, action string) error {
	states := map[string]int{"on": 2, "cycle": 5, "off": 8, "reset": 10}
	state, ok := states[action]
	if !ok {
		return errors.New("unsupported AMT power action")
	}
	uri := cimNS + "CIM_PowerManagementService"
	selectors := `<w:SelectorSet><w:Selector Name="Name">Intel(r) AMT Power Management Service</w:Selector></w:SelectorSet>`
	body := `<h:RequestPowerStateChange_INPUT xmlns:h="` + uri + `"><h:PowerState>` + strconv.Itoa(state) + `</h:PowerState>` +
		`<h:ManagedElement><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address>` +
		`<a:ReferenceParameters><w:ResourceURI>` + cimNS + `CIM_ComputerSystem</w:ResourceURI>` +
		`<w:SelectorSet><w:Selector Name="CreationClassName">CIM_ComputerSystem</w:Selector>` +
		`<w:Selector Name="Name">ManagedSystem</w:Selector></w:SelectorSet></a:ReferenceParameters>` +
		`</h:ManagedElement></h:RequestPowerStateChange_INPUT>`
	raw, err := a.soap(ctx, uri+"/RequestPowerStateChange", uri, body, selectors)
	if err != nil {
		return err
	}
	if xmlField(raw, "ReturnValue") != "0" {
		return errors.New("AMT power request was refused")
	}
	return nil
}

func soapBody(raw []byte) (string, error) {
	start := bytes.Index(raw, []byte(":Body>"))
	if start < 0 {
		return "", errors.New("AMT WS-MAN body missing")
	}
	start += len(":Body>")
	end := bytes.LastIndex(raw, []byte("</s:Body>"))
	if end < start {
		return "", errors.New("AMT WS-MAN body missing")
	}
	return string(raw[start:end]), nil
}

func (a *amtWSMan) bootToSetup(ctx context.Context) (resultErr error) {
	uri := amtNS + "AMT_BootSettingData"
	raw, err := a.get(ctx, uri)
	if err != nil {
		return err
	}
	instance, err := soapBody(raw)
	if err != nil {
		return err
	}
	if len(biosSetupField.FindAllStringIndex(instance, -1)) != 1 {
		return errors.New("AMT BIOSSetup field missing")
	}
	if len(bootRoot.FindAllStringIndex(instance, -1)) != 1 {
		return errors.New("AMT boot settings instance missing")
	}
	instance = bootRoot.ReplaceAllStringFunc(instance, func(root string) string {
		prefix := bootRoot.FindStringSubmatch(root)[1]
		if strings.Contains(root, "xmlns:"+prefix+"=") {
			return root
		}
		return strings.TrimSuffix(root, ">") + ` xmlns:` + prefix + `="` + html.EscapeString(uri) + `">`
	})
	original := instance
	instance = biosSetupField.ReplaceAllStringFunc(instance, func(field string) string {
		parts := biosSetupField.FindStringSubmatch(field)
		return "<" + parts[1] + "BIOSSetup>true</" + parts[3] + "BIOSSetup>"
	})
	// Even an uncertain Put can have armed BIOSSetup. On any later failure,
	// attempt to restore the exact original settings under a fresh bounded
	// cleanup context, including when the job's deadline has already expired.
	defer func() {
		if resultErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, restoreErr := a.soap(cleanupCtx, transferNS+"Put", uri, original, ""); restoreErr != nil {
			resultErr = errors.Join(resultErr, errors.New("AMT boot settings restoration unconfirmed"))
		}
	}()
	if _, err = a.soap(ctx, transferNS+"Put", uri, instance, ""); err != nil {
		return err
	}

	configURI := cimNS + "CIM_BootConfigSetting"
	configSelector := `<w:SelectorSet><w:Selector Name="InstanceID">Intel(r) AMT: Boot Configuration 0</w:Selector></w:SelectorSet>`
	orderBody := `<h:ChangeBootOrder_INPUT xmlns:h="` + configURI + `"><h:Source/></h:ChangeBootOrder_INPUT>`
	order, err := a.soap(ctx, configURI+"/ChangeBootOrder", configURI, orderBody, configSelector)
	if err != nil {
		return err
	}
	if xmlField(order, "ReturnValue") != "0" {
		return errors.New("AMT boot order was refused")
	}

	serviceURI := cimNS + "CIM_BootService"
	serviceSelector := `<w:SelectorSet><w:Selector Name="Name">Intel(r) AMT Boot Service</w:Selector></w:SelectorSet>`
	roleBody := `<h:SetBootConfigRole_INPUT xmlns:h="` + serviceURI + `">` +
		`<h:BootConfigSetting><a:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</a:Address>` +
		`<a:ReferenceParameters><w:ResourceURI>` + configURI + `</w:ResourceURI>` + configSelector +
		`</a:ReferenceParameters></h:BootConfigSetting><h:Role>1</h:Role></h:SetBootConfigRole_INPUT>`
	role, err := a.soap(ctx, serviceURI+"/SetBootConfigRole", serviceURI, roleBody, serviceSelector)
	if err != nil {
		return err
	}
	if xmlField(role, "ReturnValue") != "0" {
		return errors.New("AMT boot role was refused")
	}
	return a.power(ctx, "reset")
}
