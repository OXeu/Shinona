package httpserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Routes registers net/http handlers on a router using method/path patterns.
type Routes interface {
	Handle(string, http.Handler)
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}

// GinRoutes registers existing HTTP handlers directly on Gin.
type GinRoutes struct{ gin.IRoutes }

func (g GinRoutes) Handle(pattern string, handler http.Handler) {
	method, path, _ := strings.Cut(pattern, " ")
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[i] = ":" + segment[1:len(segment)-1]
		}
	}
	g.IRoutes.Handle(method, strings.Join(segments, "/"), func(c *gin.Context) {
		for _, param := range c.Params {
			c.Request.SetPathValue(param.Key, param.Value)
		}
		handler.ServeHTTP(c.Writer, c.Request)
	})
}

func (g GinRoutes) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	g.Handle(pattern, http.HandlerFunc(handler))
}
