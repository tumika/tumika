package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An internal test because it exercises the middleware directly rather than
// through the router: no route panics, so reaching this behaviour from outside
// would mean exporting something purely for the test.
func TestPanicsBecomeAnInternalError(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something went very wrong")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8737/v1/health", nil)
	recovering(logger)(panicking).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The panic value can carry anything — a query, a path, a struct dump. It
	// belongs in the log, not in a response.
	if strings.Contains(rec.Body.String(), "something went very wrong") {
		t.Errorf("the panic value leaked into the response: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "something went very wrong") {
		t.Errorf("the panic was not logged:\n%s", logs.String())
	}
}

// http.ErrAbortHandler is the documented way to abandon a response on purpose.
// Swallowing it would turn a deliberate abort into a bogus 500.
func TestDeliberateAbortsArePropagated(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	aborting := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	defer func() {
		if p := recover(); p == nil {
			t.Error("http.ErrAbortHandler must propagate, not become a 500")
		}
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8737/v1/health", nil)
	recovering(logger)(aborting).ServeHTTP(rec, req)
}
