package web

import (
	"fmt"
	"net/http"
	"strings"
)

// guard makes the local server usable only from its own page:
//   - Host must be 127.0.0.1/localhost on our port (blocks DNS-rebinding),
//   - browsers' Sec-Fetch-Site must be same-origin or none (blocks other sites' forms, images and fetches),
//   - API calls must carry X-TGDL: 1, a custom header that cross-site requests can't send without a CORS preflight
//     this server never approves. Image and video endpoints (<img>/<video> can't set headers) rely on the first two checks.
func guard(port int, next http.Handler) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !isImage(r) && r.Header.Get("X-TGDL") != "1" {
			http.Error(w, "missing X-TGDL header", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func isImage(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(strings.HasPrefix(r.URL.Path, "/api/thumb/") || strings.HasPrefix(r.URL.Path, "/api/stream/") || r.URL.Path == "/api/login/qr.png")
}
