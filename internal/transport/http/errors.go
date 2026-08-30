package http

import (
	"errors"
	"log/slog"
	"strconv"

	"nexusmail/internal/ports"

	"github.com/gin-gonic/gin"
)

func idParam(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		fail(c, 400, "invalid_id", name+" must be a positive integer", nil)
		return 0, false
	}
	return id, true
}

// optionalInt64 reads a positive integer query parameter. The bool reports
// whether the request is still valid, not whether the parameter was present — an
// absent parameter yields (nil, true) and a malformed one yields (nil, false)
// after the 400 has been written.
//
// It used to return false for both cases, which meant a malformed filter wrote
// its 400 and then let the handler run on: the response body ended up holding the
// error envelope followed by a second JSON document, and the query still executed
// with the filter silently dropped, so a client that mistyped account_id received
// a 400 status stapled to the whole unfiltered feed.
func optionalInt64(c *gin.Context, key string) (*int64, bool) {
	raw := c.Query(key)
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		fail(c, 400, "invalid_filter", key+" must be a positive integer", nil)
		return nil, false
	}
	return &value, true
}
func fail(c *gin.Context, status int, code, message string, details any) {
	requestID, _ := c.Get("request_id")
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "request_id": requestID, "details": details}})
}

// writeError maps a failure to a status from the class its producer declared, via
// the ports sentinels. It used to sniff substrings — "invalid" or "required" meant
// 400, "offline" meant 503 — which failed in both directions: only the 500 branch
// redacts, so any internal error whose text happened to contain one of those words
// was echoed to the client verbatim, and any deliberate 400 whose wording drifted
// silently became a redacted 500.
//
// Anything unclassified is an internal failure by definition: it gets 500 and its
// text is dropped rather than being guessed at.
func writeError(c *gin.Context, err error) {
	status, code := 500, "internal_error"
	switch {
	case errors.Is(err, ports.ErrNotFound):
		status, code = 404, "not_found"
	case errors.Is(err, ports.ErrConflict):
		status, code = 409, "conflict"
	case errors.Is(err, ports.ErrInvalidInput):
		status, code = 400, "invalid_request"
	case errors.Is(err, ports.ErrUnavailable):
		status, code = 503, "provider_unavailable"
	}
	message := err.Error()
	if status == 500 {
		slog.Error("request failed", "request_id", c.GetString("request_id"), "path", c.FullPath(), "error", err)
		message = "internal server error"
	}
	fail(c, status, code, message, nil)
}
