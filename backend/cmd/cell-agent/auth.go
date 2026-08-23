package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth wraps a handler with constant-time bearer-token verification. The token
// is never logged. A missing/short/wrong token → 401.
func (a *Agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(h, prefix))
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next(w, r)
	}
}
