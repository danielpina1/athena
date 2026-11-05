package web

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// maxEnrollLinkTTL is the longest a link requested over HTTP may be good for.
const maxEnrollLinkTTL = 24 * time.Hour

// EnrollLink answers POST /auth/enroll-link: a single-use enrollment link for another device, the same /enroll#<token>
// form `athena web enroll` prints. It needs the configured Origin, a live session and a passkey assertion within the
// last 5 minutes (403 reauth_required without one). The body {"ttlSeconds":N} is optional (1...86400, default
// DefaultEnrollTTL). The token travels in the response body only: it is not logged and, in the fragment, never sent
// back to the server by a browser. This is HTTP, not a frame, so no enrollment token ever rides the dashboard link.
func (a *Auth) EnrollLink(w http.ResponseWriter, r *http.Request) {
	if !a.checkOrigin(w, r) {
		return
	}
	s, ok := a.Session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
		return
	}
	if !a.RecentReauth(s) {
		fail(w, http.StatusForbidden, "reauth_required", "confirm with your passkey first")
		return
	}
	ttl := DefaultEnrollTTL
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid", "invalid request")
		return
	}
	if len(body) > 0 {
		var req struct {
			TTLSeconds *int64 `json:"ttlSeconds"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			fail(w, http.StatusBadRequest, "invalid", "invalid request")
			return
		}
		if req.TTLSeconds != nil {
			if *req.TTLSeconds < 1 || *req.TTLSeconds > int64(maxEnrollLinkTTL/time.Second) {
				fail(w, http.StatusBadRequest, "invalid", "ttlSeconds must be between 1 and 86400")
				return
			}
			ttl = time.Duration(*req.TTLSeconds) * time.Second
		}
	}
	expires := time.Now().Add(ttl)
	token, err := issueEnrollTokenUntil(a.st, expires)
	if err != nil {
		fail(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url":       a.origin + "/enroll#" + token,
		"expiresAt": expires.UTC().Format(time.RFC3339),
	})
}
