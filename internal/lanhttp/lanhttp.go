// Package lanhttp holds the plumbing for this module's LAN-facing HTTP receiver
// (auv3): a /healthz handler, atomic file writes, and a leak-free error
// responder. The listener faces an untrusted LAN, so this centralizes the
// "bound the resources a slow/hostile client can tie up" and "log the cause,
// return only a generic status" rules.
//
// ponytail: deliberately duplicated from mcp-midi-controller's internal/lanhttp
// rather than carved into a fourth shared module. The app keeps its own copy for
// its app-only receivers (aumreceiver, audiotap, diagnostics); only the auv3
// receiver moved here. Upgrade path if these diverge or a third consumer
// appears: promote lanhttp to its own tiny module both depend on.
package lanhttp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Healthz is the receivers' shared liveness endpoint, so the iPad app can test
// connectivity before POSTing.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

// Serve runs handler on addr until ctx is cancelled, then shuts the server down
// gracefully (5s drain). The timeouts and header cap are deliberately
// conservative because the listener faces the LAN.
func Serve(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// Responder turns internal failures into HTTP responses that log the detailed
// cause server-side (prefixed with Prefix) but return only a generic,
// status-derived message to the (untrusted) client, so filesystem paths and
// internal error strings never leak over the network.
type Responder struct {
	// Prefix labels the server-side log line, e.g. "aum-session receiver".
	Prefix string
}

// Error logs the formatted cause and writes the generic message for code.
func (r Responder) Error(w http.ResponseWriter, code int, format string, args ...any) {
	log.Printf("%s: %s", r.Prefix, fmt.Sprintf(format, args...))
	http.Error(w, ClientMessage(code), code)
}

// ClientMessage is the generic, leak-free body for a status code.
func ClientMessage(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "bad request"
	case http.StatusNotFound:
		return "not found"
	case http.StatusRequestEntityTooLarge:
		return "request too large"
	default:
		return "internal error"
	}
}

// DecodeErrStatus maps a body-read/decode error to a status: a body that
// exceeds the MaxBytesReader cap is 413, anything else is a 400.
func DecodeErrStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// WriteFileAtomic writes data to a temp file in the same directory and renames
// it into place, so a concurrent reader (or a second write for the same path)
// never observes a half-written file. The temp file is cleaned up on error.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
