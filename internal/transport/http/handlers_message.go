package http

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mailparser "nexusmail/internal/mail"
	"nexusmail/internal/ports"

	"github.com/gin-gonic/gin"
)

func (s *Server) listMessages(c *gin.Context) {
	filter := ports.MessageFilter{Folder: c.Query("folder"), Query: strings.TrimSpace(c.Query("query")), Cursor: c.Query("cursor")}
	filter.Limit, _ = strconv.Atoi(c.Query("limit"))
	if value, ok := optionalInt64(c, "account_id"); ok {
		filter.AccountID = value
	}
	if value, ok := optionalInt64(c, "mailbox_id"); ok {
		filter.MailboxID = value
		_ = s.sync.RequestMailbox(c.Request.Context(), *value)
	}
	if raw := c.Query("is_read"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			fail(c, 400, "invalid_filter", "is_read must be true or false", nil)
			return
		}
		filter.IsRead = &value
	}
	page, err := s.messages.List(c.Request.Context(), filter)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, page)
}

// markMessagesRead marks the current view read. The scope is expressed with the
// same query parameters as the feed so "everything I can see" cannot drift
// between the list and the button acting on it.
func (s *Server) markMessagesRead(c *gin.Context) {
	filter := ports.MessageFilter{Folder: c.Query("folder"), Query: strings.TrimSpace(c.Query("query"))}
	if value, ok := optionalInt64(c, "account_id"); ok {
		filter.AccountID = value
	}
	if value, ok := optionalInt64(c, "mailbox_id"); ok {
		filter.MailboxID = value
	}
	result, err := s.messages.MarkRead(c.Request.Context(), filter)
	if err != nil {
		// A partial success still changed state on the provider and locally, so it
		// is reported as success with a flag rather than thrown away as an error.
		if result.Updated == 0 {
			writeError(c, err)
			return
		}
		c.JSON(200, gin.H{"updated": result.Updated, "capped": result.Capped, "partial": true})
		return
	}
	c.JSON(200, gin.H{"updated": result.Updated, "capped": result.Capped})
}

func (s *Server) getMessage(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	message, attachments, err := s.messages.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	status := http.StatusOK
	if message.BodyState != "ready" {
		done := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					// gin.Recovery only covers the request goroutine, not this
					// background fetch. A panic in MIME parsing or in the FTS5
					// trigger path here would crash the whole process.
					done <- fmt.Errorf("body fetch panicked: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(s.appCtx, 30*time.Second)
			defer cancel()
			done <- s.sync.FetchBody(ctx, id)
		}()
		select {
		case err := <-done:
			if err == nil {
				message, attachments, _ = s.messages.Get(c.Request.Context(), id)
			} else {
				status = http.StatusAccepted
			}
		case <-time.After(3 * time.Second):
			status = http.StatusAccepted
		case <-c.Request.Context().Done():
			return
		}
	}
	body := gin.H{"message": message, "attachments": attachments}
	// The code is derived on read rather than stored: it is cheap to recompute and
	// a column would need a migration the runner cannot apply. Only the detail
	// endpoint pays for it — the feed would run this over a whole page of bodies.
	if code, ok := mailparser.DetectOTP(message.Subject, message.BodyText, message.BodyHTML); ok {
		body["otp_code"] = code
	}
	c.JSON(status, body)
}

func (s *Server) patchMessage(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var input struct {
		IsRead    *bool `json:"is_read"`
		IsStarred *bool `json:"is_starred"`
		Archive   bool  `json:"archive"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	message, err := s.messages.Patch(c.Request.Context(), id, ports.MessagePatch{IsRead: input.IsRead, IsStarred: input.IsStarred}, input.Archive)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, message)
}

func (s *Server) downloadAttachment(c *gin.Context) {
	messageID, ok := idParam(c, "id")
	if !ok {
		return
	}
	attachmentID, ok := idParam(c, "attachment_id")
	if !ok {
		return
	}
	blob, attachment, err := s.sync.FetchAttachment(c.Request.Context(), messageID, attachmentID)
	if err != nil {
		writeError(c, err)
		return
	}
	reader, err := s.blobs.Open(c.Request.Context(), blob)
	if err != nil {
		writeError(c, err)
		return
	}
	defer reader.Close()
	contentType := attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)
	c.Header("Content-Length", strconv.FormatInt(blob.SizeBytes, 10))
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(attachment.Filename)}))
	_, _ = io.Copy(c.Writer, reader)
}
