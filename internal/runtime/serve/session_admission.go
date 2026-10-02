package serve

import (
	"net/http"

	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
)

func sessionAdmissionMux(g *sessionadmission.Gate, next http.Handler) http.Handler {
	if g == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(sessionadmission.WithGate(r.Context(), g)))
	})
}
