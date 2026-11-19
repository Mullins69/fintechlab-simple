package httputilx

import (
	"net/http"
	"strings"
)

// Forwarded is the first entry of a forwarded header (X-Forwarded-Host,
// X-Forwarded-Proto, X-Forwarded-Prefix), trimmed. Each proxy in a chain
// appends its own entry, so after two the header reads
// "lab.example, b4b"; the first is what the client used. Empty when the
// header is absent.
func Forwarded(r *http.Request, name string) string {
	first, _, _ := strings.Cut(r.Header.Get(name), ",")
	return strings.TrimSpace(first)
}
