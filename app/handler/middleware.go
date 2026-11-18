package handler

import (
	"log"
	"net/http"
	"time"
)

// AccessLog logs one line per request, including the latency.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("http: %s %s -> %d in %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

// Recoverer turns a panic in a handler into a 500 instead of a dropped
// connection. The fetcher goroutines have their own recovery; a panic there
// used to take the whole process down.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("http: panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// csp reflects what the templates actually use: one same-origin stylesheet, no
// inline styles, no scripts at all. Feed content is rendered as text, so remote
// images are not needed either - and allowing them would let a feed make the
// operator's browser request arbitrary URLs.
const csp = "default-src 'self'; script-src 'none'; img-src 'self'; style-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; frame-src 'none'"

// SecurityHeaders sets the hardening headers for the admin UI. The UI is
// authenticated (see Handler.Routes); HSTS is only emitted when the operator
// declared that the site is served over TLS, since sending it over plain HTTP
// would break the very next request.
func SecurityHeaders(secure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Content-Security-Policy", csp)
			if secure {
				h.Set("Strict-Transport-Security", "max-age=15552000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
