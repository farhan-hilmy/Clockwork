package httpapi

import (
	_ "embed"
	"net/http"
)

// openAPISpec describes /api/v1. TestOpenAPIMatchesRouter keeps it in step with the router.
//
//go:embed openapi.json
var openAPISpec []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(openAPISpec)
}
