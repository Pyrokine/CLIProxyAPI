package httpwire

import (
	"net/http"
	"reflect"
)

// FlushResponse preserves middleware flushing and reports transport failures when available.
func FlushResponse(writer http.ResponseWriter) error {
	for {
		if flusher, ok := writer.(interface{ FlushError() error }); ok {
			return flusher.FlushError()
		}
		if flusher, ok := writer.(http.Flusher); ok {
			if !isGinResponseWriter(writer) {
				flusher.Flush()
				return nil
			}
			writer.(interface{ WriteHeaderNow() }).WriteHeaderNow()
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return http.NewResponseController(writer).Flush()
		}
		writer = unwrapper.Unwrap()
	}
}

func isGinResponseWriter(writer http.ResponseWriter) bool {
	t := reflect.TypeOf(writer)
	return t != nil && t.Kind() == reflect.Pointer &&
		t.Elem().PkgPath() == "github.com/gin-gonic/gin" && t.Elem().Name() == "responseWriter"
}
