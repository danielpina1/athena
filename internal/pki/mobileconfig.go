package pki

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"
)

// MobileConfig is an unsigned Apple configuration profile that installs the CA as a trusted root. Its UUIDs derive
// from the CA fingerprint, so serving it twice gives the same profile and iOS replaces rather than duplicates it.
func MobileConfig(ca *CA) []byte {
	name := "Athena CA (" + ca.host + ")"
	id := "app.athena.ca." + strings.ReplaceAll(strings.ToLower(ca.Fingerprint()[:23]), ":", "")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
`)
	entry := func(indent, key, kind, val string) {
		fmt.Fprintf(&b, "%s<key>%s</key>\n%s<%s>%s</%s>\n", indent, key, indent, kind, val, kind)
	}
	const in = "\t\t\t"
	entry(in, "PayloadCertificateFileName", "string", "athena-ca.cer")
	entry(in, "PayloadContent", "data", base64.StdEncoding.EncodeToString(ca.cert.Raw))
	entry(in, "PayloadDescription", "string", "Trusts certificates Athena issues for "+esc(ca.host))
	entry(in, "PayloadDisplayName", "string", esc(name))
	entry(in, "PayloadIdentifier", "string", id+".root")
	entry(in, "PayloadType", "string", "com.apple.security.root")
	entry(in, "PayloadUUID", "string", uuidFor(ca, "root"))
	entry(in, "PayloadVersion", "integer", "1")
	b.WriteString("\t\t</dict>\n\t</array>\n")
	entry("\t", "PayloadDescription", "string", "Installs the certificate authority for your Athena dashboard")
	entry("\t", "PayloadDisplayName", "string", esc(name))
	entry("\t", "PayloadIdentifier", "string", id)
	entry("\t", "PayloadOrganization", "string", "Athena")
	entry("\t", "PayloadType", "string", "Configuration")
	entry("\t", "PayloadUUID", "string", uuidFor(ca, "profile"))
	entry("\t", "PayloadVersion", "integer", "1")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// uuidFor derives a stable version-4-shaped UUID from the CA fingerprint and a purpose label.
func uuidFor(ca *CA, purpose string) string {
	h := sha256.Sum256([]byte(ca.Fingerprint() + "/" + purpose))
	h[6] = h[6]&0x0f | 0x40
	h[8] = h[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16]))
}
