package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"metadata-api/internal/metadata"
)

func TestAdmissionRemainsOwnedWhilePanicRecoveryWritesItsResponse(t *testing.T) {
	store := &faultStore{MetadataStore: newMemory(t), panicGet: true}
	handler := boundedHandler(t, store, 5*time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	writer := &blockedResponseWriter{
		ResponseWriter: httptest.NewRecorder(), entered: entered, release: release,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(writer, localRequest("GET", "/v1/maintainers/"+metadata.NewUUID(), nil))
	}()
	awaitEntry(t, entered)
	assertOverload(t, request(handler, "GET", "/v1/maintainers", "", ""))
	assertStatus(t, request(handler, "GET", "/healthz", "", ""), 200)
	close(release)
	awaitEntry(t, done)
	assertStatus(t, request(handler, "GET", "/v1/maintainers", "", ""), 200)
}

type blockedResponseWriter struct {
	http.ResponseWriter
	entered chan struct{}
	release <-chan struct{}
}

func (w *blockedResponseWriter) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseWriter.Write(data)
}
