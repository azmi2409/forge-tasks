package api

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azmi2409/forge-tasks/bus"
	"github.com/azmi2409/forge-tasks/store"
)

type sseFrame struct {
	id    string
	event string
	data  string
}

// readFrame reads one SSE frame (id/event/data + blank line).
func readFrame(t *testing.T, r *bufio.Reader) sseFrame {
	t.Helper()
	var f sseFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return f // blank line ends the frame
		}
		if strings.HasPrefix(line, ":") {
			continue // comment/ping
		}
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			f.id = v
		} else if v, ok := strings.CutPrefix(line, "event: "); ok {
			f.event = v
		} else if v, ok := strings.CutPrefix(line, "data: "); ok {
			f.data = v
		}
	}
}

func TestEventStream(t *testing.T) {
	st := store.NewMemoryStore()
	s := NewServer(st, nil)
	s.SetBus(bus.New())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Seed events first, then open the stream with cursor=1: replay is
	// deterministic (no publish/subscribe race).
	doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "FT", "name": "Forge"})
	doRequest(t, s, "POST", "/api/v1/work-items",
		map[string]any{"project_key": "FT", "title": "streamed task"})

	resp, err := http.Get(ts.URL + "/api/v1/events?cursor=1")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%s", ct)
	}
	reader := bufio.NewReader(resp.Body)

	// project.created, then work_item.created, via replay.
	f1 := readFrame(t, reader)
	f2 := readFrame(t, reader)
	if f1.event != bus.TypeProjectCreated || f2.event != bus.TypeWorkItemCreated {
		t.Fatalf("frames: %+v %+v", f1, f2)
	}
	if f1.id == "" || f2.id == "" || !strings.Contains(f2.data, "streamed task") {
		t.Fatalf("frames: %+v %+v", f1, f2)
	}

	// The stream stays open: a new event arrives live.
	doRequest(t, s, "POST", "/api/v1/work-items",
		map[string]any{"project_key": "FT", "title": "live task"})
	f3 := readFrame(t, reader)
	if f3.event != bus.TypeWorkItemCreated || !strings.Contains(f3.data, "live task") {
		t.Fatalf("live frame: %+v", f3)
	}
}

func TestEventStreamCursor(t *testing.T) {
	st := store.NewMemoryStore()
	s := NewServer(st, nil)
	b := bus.New()
	s.SetBus(b)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Seed two events, note the cursor, then add a third.
	doRequest(t, s, "POST", "/api/v1/projects", map[string]string{"key": "FT", "name": "Forge"})
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{"project_key": "FT", "title": "one"})
	cursor := b.CurrentSeq()
	doRequest(t, s, "POST", "/api/v1/work-items", map[string]any{"project_key": "FT", "title": "two"})

	// Replay from the cursor: only "two" arrives, then the stream stays open.
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/events?cursor=%d", ts.URL, cursor+1))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp.Body.Close()
	f := readFrame(t, bufio.NewReader(resp.Body))
	if f.event != bus.TypeWorkItemCreated || !strings.Contains(f.data, `"two"`) {
		t.Fatalf("frame=%+v", f)
	}

	// Flood the ring so cursor=1 falls out, then re-request: 400, not a
	// hanging stream.
	for i := 0; i < 2100; i++ {
		b.Publish(bus.TypeWorkItemCreated, nil)
	}
	resp2, err := http.Get(fmt.Sprintf("%s/api/v1/events?cursor=1", ts.URL))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("evicted cursor: status=%d, want 400", resp2.StatusCode)
	}
}
