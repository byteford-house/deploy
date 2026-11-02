package server

import (
	"log"
	"net/http"
	"time"
)

// loggingMiddleware wraps every request with one access-log line —
// method, path, remote address, status code, duration — printed via the
// standard log package (stderr, timestamped) so running `serve` shows
// what's actually hitting the server instead of staying silent until
// something breaks. For long-lived requests (the SSE stream), this line
// is only printed once the connection closes; handleJobStream logs its
// own connect/disconnect lines separately so those don't go unseen for
// the whole lifetime of the stream.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// The OIDC callback's query holds the one-time authorization
		// code and state; keep them out of the logs.
		uri := r.URL.RequestURI()
		if r.URL.Path == callbackPath {
			uri = r.URL.Path + "?(redacted)"
		}
		log.Printf("%s %s %s -> %d (%s)", r.RemoteAddr, r.Method, uri, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter captures the status code a handler wrote, since a plain
// http.ResponseWriter doesn't expose it after the fact.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write must also mark wroteHeader — a handler that calls Write without
// an explicit WriteHeader gets an implicit 200, matching net/http's own
// default behavior, and without this override that implicit 200 would
// never get recorded in status.
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets statusWriter still satisfy http.Flusher when the
// underlying ResponseWriter does — handleJobStream's SSE handler needs
// to flush after every write, and wrapping the writer here must not
// silently break that.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
