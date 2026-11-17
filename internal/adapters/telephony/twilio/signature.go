package twilio

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// validSignature checks Twilio's X-Twilio-Signature (HMAC-SHA1 over the request URL
// followed by every form parameter, sorted by name). requestURL must be the public URL
// Twilio was configured to call, not r.Host (which is often an internal tunnel address).
func validSignature(authToken, requestURL, signature string, params url.Values) bool {
	if authToken == "" || signature == "" || requestURL == "" {
		return false
	}
	if _, err := url.Parse(requestURL); err != nil {
		return false
	}

	var b strings.Builder
	b.WriteString(requestURL)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		values := params[k]
		v := ""
		if len(values) > 0 {
			v = values[0]
		}
		b.WriteString(k)
		b.WriteString(v)
	}
	signed := b.String()

	if hmac.Equal([]byte(signatureFor(authToken, signed)), []byte(signature)) {
		return true
	}
	// Twilio's backend has historically signed with and without an explicit default port.
	// Accept both spellings, exactly like the official validators do.
	for _, alt := range portVariants(requestURL) {
		altSigned := alt + signed[len(requestURL):]
		if hmac.Equal([]byte(signatureFor(authToken, altSigned)), []byte(signature)) {
			return true
		}
	}
	return false
}

func signatureFor(token, signed string) string {
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(signed))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// signedURL reconstructs the URL Twilio signed. Our webhook URLs carry the call ID in
// the path and no query string, but include RawQuery defensively.
func signedURL(r *http.Request, publicBaseURL string) string {
	u := strings.TrimRight(publicBaseURL, "/") + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

// portVariants returns the same URL with the default port added or removed.
func portVariants(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	port := u.Port()
	switch {
	case port == "" && defaultPort(u.Scheme) != "":
		u2 := *u
		u2.Host = net.JoinHostPort(host, defaultPort(u.Scheme))
		return []string{u2.String()}
	case port == defaultPort(u.Scheme):
		u2 := *u
		u2.Host = host
		return []string{u2.String()}
	default:
		return nil
	}
}

func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
