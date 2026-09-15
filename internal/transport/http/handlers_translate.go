package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// translateClient never follows redirects: a misconfigured or hostile
// NEXUSMAIL_TRANSLATE_URL must not be able to bounce the message body to a
// third host via a 302.
var translateClient = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

const maxTranslateSourceRunes = 12_000

// translateMessage forwards the message body to an optional deployment-configured
// HTTP translator. The row is never rewritten: the client shows the result beside
// the original, so a bad translation cannot destroy the source of truth.
func (s *Server) translateMessage(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var input struct {
		TargetLang string `json:"target_lang"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || strings.TrimSpace(input.TargetLang) == "" {
		fail(c, 400, "invalid_request", "target_lang is required", nil)
		return
	}
	lang := strings.TrimSpace(input.TargetLang)
	if len(lang) > 16 || strings.ContainsAny(lang, " \t\r\n\"\\") {
		fail(c, 400, "invalid_request", "target_lang is malformed", nil)
		return
	}
	if s.cfg.TranslateURL == "" {
		fail(c, 400, "translate_not_configured", "gateway has no NEXUSMAIL_TRANSLATE_URL configured", nil)
		return
	}
	message, _, err := s.messages.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	source := message.BodyText
	if source == "" {
		source = message.Snippet
	}
	source = strings.TrimSpace(source)
	if source == "" {
		fail(c, 400, "empty_body", "message has no text body to translate", nil)
		return
	}
	// Bound the outbound payload: a multi-megabyte newsletter should not become
	// a multi-megabyte POST to an external service the operator configured.
	if runes := []rune(source); len(runes) > maxTranslateSourceRunes {
		source = string(runes[:maxTranslateSourceRunes])
	}
	// The HTTP call is charged to a timeout under appCtx so a hung translator
	// cannot pin the request forever, but the caller's disconnect still cancels
	// through the request context for the load of the message above.
	ctx, cancel := context.WithTimeout(s.appCtx, 20*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]string{"text": source, "target_lang": lang})
	if err != nil {
		writeError(c, err)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TranslateURL, bytes.NewReader(payload))
	if err != nil {
		writeError(c, err)
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := translateClient.Do(request)
	if err != nil {
		fail(c, 502, "translate_failed", "translation service is unreachable", nil)
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		fail(c, 502, "translate_failed", "translation service returned an unreadable body", nil)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fail(c, 502, "translate_failed", fmt.Sprintf("translation service answered HTTP %d", response.StatusCode), nil)
		return
	}
	var result struct {
		Text               string `json:"text"`
		DetectedSourceLang string `json:"detected_source_lang"`
	}
	if err := json.Unmarshal(body, &result); err != nil || strings.TrimSpace(result.Text) == "" {
		fail(c, 502, "translate_failed", "translation service returned no text", nil)
		return
	}
	c.JSON(200, gin.H{"text": result.Text, "detected_source_lang": result.DetectedSourceLang})
}
