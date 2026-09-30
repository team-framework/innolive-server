package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// clientAssets는 /client/에서 제공하는 브라우저 테스트 뷰어(index.html + app.js +
// styles.css)를 담는다. Python 기준 서버와 같은 API 계약이라 같은 정적 뷰어가 이 Go
// 서버를 그대로 구동한다.
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
