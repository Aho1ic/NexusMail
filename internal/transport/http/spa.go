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
		if apiPath(c.Request.URL.Path) {
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
				// Only files vite emitted under assets/ carry a content hash, so only they
				// can be pinned: their bytes can never change under the same name. Anything
				// else in the bundle was copied verbatim from web/public — sw.js, the icons —
				// keeps its name across builds and stays on the no-cache default.
				//
				// The check is made here rather than on the /assets/ request prefix because
				// it is only sound once the file is known to exist. On the prefix alone a
				// stale asset URL — exactly what a cached shell requests after a rebuild —
				// falls through to the shell below and would pin index.html for a year,
				// stranding that tab permanently.
				if strings.HasPrefix(path, "assets/") {
					c.Header("Cache-Control", "public, max-age=31536000, immutable")
				}
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
