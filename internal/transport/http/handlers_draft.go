package http

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nexusmail/internal/domain"
	draftservice "nexusmail/internal/service/draft"

	"github.com/gin-gonic/gin"
)

func (s *Server) listDrafts(c *gin.Context) {
	items, err := s.drafts.List(c.Request.Context(), c.Query("status"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"items": items})
}
func (s *Server) getDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	draft, attachments, err := s.drafts.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(200, gin.H{"draft": draft, "attachments": attachments})
}
func (s *Server) createDraft(c *gin.Context) {
	var input draftservice.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	draft, err := s.drafts.Create(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(201, draft)
}
func (s *Server) updateDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(strings.Trim(c.GetHeader("If-Match"), `"`), 10, 64)
	if err != nil {
		fail(c, 428, "revision_required", "If-Match revision is required", nil)
		return
	}
	var input draftservice.Input
	if err := c.ShouldBindJSON(&input); err != nil {
		fail(c, 400, "invalid_request", err.Error(), nil)
		return
	}
	draft, err := s.drafts.Update(c.Request.Context(), id, revision, input)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("ETag", fmt.Sprintf(`"%d"`, draft.Revision))
	c.JSON(200, draft)
}
func (s *Server) deleteDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := s.drafts.Delete(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.Status(204)
}

func (s *Server) addDraftAttachment(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxOutboundBytes)
	file, err := c.FormFile("file")
	if err != nil {
		fail(c, 400, "invalid_attachment", err.Error(), nil)
		return
	}
	opened, err := file.Open()
	if err != nil {
		writeError(c, err)
		return
	}
	defer opened.Close()
	blob, err := s.blobs.Put(c.Request.Context(), opened, "durable")
	if err != nil {
		writeError(c, err)
		return
	}
	contentType := file.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	attachment := domain.DraftAttachment{DraftID: id, BlobID: blob.ID, Filename: filepath.Base(file.Filename), ContentType: contentType, SizeBytes: blob.SizeBytes, CreatedAt: time.Now().UnixMilli()}
	if err := s.repo.AddDraftAttachment(c.Request.Context(), &attachment); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(201, attachment)
}
func (s *Server) deleteDraftAttachment(c *gin.Context) {
	draftID, ok := idParam(c, "id")
	if !ok {
		return
	}
	attachmentID, ok := idParam(c, "attachment_id")
	if !ok {
		return
	}
	if err := s.repo.DeleteDraftAttachment(c.Request.Context(), draftID, attachmentID); err != nil {
		writeError(c, err)
		return
	}
	c.Status(204)
}
func (s *Server) sendDraft(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := s.sender.Queue(c.Request.Context(), id); err != nil {
		writeError(c, err)
		return
	}
	c.JSON(202, gin.H{"id": id, "status": "queued"})
}
