package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/azmi2409/forge-tasks/bus"
)

// handleEvents streams bus events as Server-Sent Events.
//
// Query params:
//   - cursor: replay from this sequence number (Last-Event-ID also works).
//     Omit for live-only. A cursor older than the retained ring gets a
//     400: the client must resync with a full list instead.
//
// Wire format per event:
//
//	id: <seq>
//	event: <type>
//	data: <json>
//
// A ":ping" comment goes out every 25s to keep idle connections alive.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, "bus_unavailable",
			"event bus not configured on this server")
		return
	}
	var fromSeq uint64
	if c := r.URL.Query().Get("cursor"); c == "" {
		fromSeq = 0 // live-only
	} else {
		n, err := strconv.ParseUint(c, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error",
				"cursor: must be a non-negative integer")
			return
		}
		fromSeq = n
	}
	// Last-Event-ID (reconnects) wins over the query param when both given.
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if n, err := strconv.ParseUint(id, 10, 64); err == nil {
			fromSeq = n
		}
	}

	ch, err := s.bus.Subscribe(r.Context(), fromSeq)
	if errors.Is(err, bus.ErrCursorTooOld) {
		writeError(w, http.StatusBadRequest, "cursor_too_old",
			"cursor fell out of the replay window; resync with a full list")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "subscribe failed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// Flush headers immediately so clients know the stream is live
	// (otherwise they'd wait for the first event or ping).
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ":ping\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				// Fell behind; the client reconnects with its cursor.
				return
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				data = []byte(`{"error":"unmarshalable event data"}`)
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data)
			flusher.Flush()
		}
	}
}
