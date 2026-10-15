package wsgateway

import (
	"net/url"
	"strings"
)

const redacted = "REDACTED"

// RedactQuery returns rawQuery with the value of every ticket parameter replaced by REDACTED
// (§7.3: access logs and traces must never carry the one-time ticket). Other parameters, their
// order and their encoding are left as they were. The key is matched after percent-decoding and
// without regard to case, so "Ticket=" and "tick%65t=" are redacted too.
func RedactQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	for i, p := range pairs {
		key, _, _ := strings.Cut(p, "=")
		name := key
		if k, err := url.QueryUnescape(key); err == nil {
			name = k
		}
		if strings.EqualFold(name, "ticket") {
			pairs[i] = key + "=" + redacted
		}
	}
	return strings.Join(pairs, "&")
}
