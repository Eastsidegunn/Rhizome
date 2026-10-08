package workspace

import (
	"io"
	"net/http"

	"rhizome/internal/events"
)

func storePoisoned(store events.Port) bool {
	latch, ok := store.(events.PoisonLatch)
	return ok && latch.Poisoned()
}

func servePoisoned(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, events.ErrPoisoned.Error())
}
