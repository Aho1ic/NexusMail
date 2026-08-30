package http

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"nexusmail/internal/transport/http/static"

	"github.com/gin-gonic/gin"
)

func (s *Server) mountSPA(router *gin.Engine) {
	root, err := fs.Sub(static.Files, "dist")
	if err != nil {
		return
	}
	files := http.FileServer(http.FS(root))
	router.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			fail(c, 404, "not_found", "route not found", nil)
			return
		}
		path := strings.TrimPrefix(filepath.Clean(c.Request.URL.Path), "/")
		if path != "." {
			if _, err := fs.Stat(root, path); err == nil {
				files.ServeHTTP(c.Writer, c.Request)
				return
			}
		}
		index, err := fs.ReadFile(root, "index.html")
		if err != nil {
			c.Status(404)
			return
		}
		c.Data(200, "text/html; charset=utf-8", index)
	})
}
