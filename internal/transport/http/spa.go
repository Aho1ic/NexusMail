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
			// The mode is the whole point: fs.Stat succeeds for directories too, so
			// matching on err alone handed /assets/ to http.FileServer, which answers
			// with an HTML index of every embedded file. Only a regular file is an
			// asset; a directory falls through to the shell like any unknown path.
			if info, err := fs.Stat(root, path); err == nil && info.Mode().IsRegular() {
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
