package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// clientAssets embeds the browser test viewer (index.html + app.js + styles.css)
// served at /client/. Same API contract as the Python reference server, so the
// same static viewer drives this Go server unchanged.
//
//go:embed static/client
var clientAssets embed.FS

func (s *Server) clientHandler() http.Handler {
	sub, err := fs.Sub(clientAssets, "static/client")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.StripPrefix("/client/", http.FileServer(http.FS(sub)))
}
