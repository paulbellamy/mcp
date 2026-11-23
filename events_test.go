package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is one JSON-RPC request seen by fakeEventsServer.
type recordedRequest struct {
	Method string
	Params json.RawMessage
	Header http.Header
}

// sseStream writes JSON-RPC messages as SSE data frames for one
// events/stream request.
type sseStream struct {
	w     http.ResponseWriter
	reqID int
	ctx   context.Context
}

func (s *sseStream) send(msg string) {
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", msg)
	s.w.(http.Flusher).Flush()
}

// notify sends an events notification tagged with this stream's
// subscription ID. params is a JSON object without the closing brace's
// _meta, which notify adds.
func (s *sseStream) notify(method, params string) {
	meta := fmt.Sprintf(`"_meta":{"io.modelcontextprotocol/subscriptionId":%d}`, s.reqID)
	if params == "" || params == "{}" {
		params = "{" + meta + "}"
	} else {
		params = strings.TrimSuffix(params, "}") + "," + meta + "}"
	}
	s.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params))
}

// close sends the StreamEventsResult final frame.
func (s *sseStream) close() {
	s.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"resultType":"complete"}}`, s.reqID))
}

// fakeEventsServer is a modern streamable-HTTP MCP server that advertises
// the Events extension (unless noExtension) and hands events/poll and
// events/stream to the test's callbacks, numbering calls from 1.
type fakeEventsServer struct {
	t           *testing.T
	events      []eventDescriptor
	noExtension bool
	poll        func(n int, params eventsPollParams) (any, *jsonrpcError)
	stream      func(n int, params eventsStreamParams, s *sseStream)

	mu       sync.Mutex
	requests []recordedRequest
	polls    int
	streams  int
}

func newFakeEventsServer(t *testing.T, f *fakeEventsServer) *httptest.Server {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeEventsServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: req.Method, Params: req.Params, Header: r.Header.Clone()})
	f.mu.Unlock()

	reply := func(result any, rpcErr *jsonrpcError) {
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID)), Error: rpcErr}
		if rpcErr == nil {
			resp.Result, _ = json.Marshal(result)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}

	switch req.Method {
	case "server/discover":
		caps := map[string]any{}
		if !f.noExtension {
			caps["extensions"] = map[string]any{extensionEvents: map[string]any{}}
		}
		reply(map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{protocolVersionModern},
			"capabilities":      caps,
		}, nil)
	case methodEventsList:
		reply(map[string]any{"resultType": "complete", "events": f.events}, nil)
	case methodEventsPoll:
		var params eventsPollParams
		_ = json.Unmarshal(req.Params, &params)
		f.mu.Lock()
		f.polls++
		n := f.polls
		f.mu.Unlock()
		reply(f.poll(n, params))
	case methodEventsStream:
		var params eventsStreamParams
		_ = json.Unmarshal(req.Params, &params)
		f.mu.Lock()
		f.streams++
		n := f.streams
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		f.stream(n, params, &sseStream{w: w, reqID: req.ID, ctx: r.Context()})
	default:
		f.t.Errorf("unexpected method %q", req.Method)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeEventsServer) requestsFor(method string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeEventsServer) sawEventsMethod() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if strings.HasPrefix(r.Method, "events/") {
			return true
		}
	}
	return false
}

// stubEventsSleep replaces eventsSleep with a recorder.
func stubEventsSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := eventsSleep
	eventsSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { eventsSleep = old })
	return &slept
}

// decodedLine is one decoded NDJSON line from follow/watch.
type decodedLine struct {
	Type        string          `json:"type"`
	EventID     string          `json:"eventId"`
	Name        string          `json:"name"`
	Cursor      *string         `json:"cursor"`
	Truncated   bool            `json:"truncated"`
	HeartbeatMs int64           `json:"heartbeatMs"`
	Error       json.RawMessage `json:"error"`
	Data        json.RawMessage `json:"data"`
	Meta        json.RawMessage `json:"_meta"`
}

func decodeEventLines(t *testing.T, stdout string) []decodedLine {
	t.Helper()
	var lines []decodedLine
	for _, raw := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if raw == "" {
			continue
		}
		var l decodedLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", raw, err)
		}
		lines = append(lines, l)
	}
	return lines
}

// lineSummary renders lines compactly for comparison: type, plus the event
// ID or cursor where present.
func lineSummary(lines []decodedLine) []string {
	var out []string
	for _, l := range lines {
		s := l.Type
		switch {
		case l.EventID != "":
			s += ":" + l.EventID
		case l.Cursor != nil:
			s += ":" + *l.Cursor
		}
		out = append(out, s)
	}
	return out
}

func strPtr(s string) *string { return &s }

const incidentSchema = `{"type":"object","properties":{
	"severity":{"type":"string"},
	"limit":{"type":"integer"},
	"region":{"type":"string","x-mcp-header":"Region"}}}`

func incidentEvent(delivery ...string) eventDescriptor {
	return eventDescriptor{
		Name:          "incident.created",
		Description:   "Fires when an incident is created",
		Delivery:      delivery,
		InputSchema:   json.RawMessage(incidentSchema),
		PayloadSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func occurrence(id string) eventOccurrence {
	return eventOccurrence{EventID: id, Name: "incident.created", Timestamp: "2026-11-23T12:00:00Z", Data: json.RawMessage(`{"id":"` + id + `"}`)}
}

func TestParseEventsArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		watch   bool
		check   func(t *testing.T, o eventsOptions)
		wantErr string
	}{
		{
			name: "poll with argument flags and cursor",
			args: []string{"srv", "incident.created", "--severity", "P1", "--limit=5", "--dry", "--cursor", "c1", "--max-age", "5m", "--max-events", "10"},
			check: func(t *testing.T, o eventsOptions) {
				if o.server != "srv" || o.event != "incident.created" {
					t.Errorf("server/event = %q/%q", o.server, o.event)
				}
				want := map[string]string{"severity": "P1", "limit": "5", "dry": "true"}
				if !reflect.DeepEqual(o.argFlags, want) {
					t.Errorf("argFlags = %v, want %v", o.argFlags, want)
				}
				if o.cursor == nil || *o.cursor != "c1" || o.maxAge != 5*time.Minute || o.maxEvents != 10 {
					t.Errorf("cursor/maxAge/maxEvents = %v/%v/%v", o.cursor, o.maxAge, o.maxEvents)
				}
				if o.minInterval != defaultMinPollInterval {
					t.Errorf("minInterval = %v, want default", o.minInterval)
				}
			},
		},
		{
			name: "empty cursor means start from now",
			args: []string{"srv", "ev", "--cursor", ""},
			check: func(t *testing.T, o eventsOptions) {
				if o.cursor != nil {
					t.Errorf("cursor = %q, want nil", *o.cursor)
				}
			},
		},
		{
			name: "follow with min interval",
			args: []string{"srv", "ev", "--follow", "--min-interval", "5s", "--cursor-file", "/tmp/c"},
			check: func(t *testing.T, o eventsOptions) {
				if !o.follow || o.minInterval != 5*time.Second || o.cursorFile != "/tmp/c" {
					t.Errorf("follow/minInterval/cursorFile = %v/%v/%q", o.follow, o.minInterval, o.cursorFile)
				}
			},
		},
		{name: "watch rejects poll-only flags", args: []string{"srv", "ev", "--follow"}, watch: true, wantErr: "--follow applies to mcp events poll"},
		{name: "missing event", args: []string{"srv"}, wantErr: "usage: mcp events poll"},
		{name: "watch usage", args: []string{"srv"}, watch: true, wantErr: "usage: mcp events watch"},
		{name: "params and flags", args: []string{"srv", "ev", "--params", "{}", "--x", "1"}, wantErr: "cannot combine --params"},
		{name: "bad duration", args: []string{"srv", "ev", "--max-age", "soon"}, wantErr: "invalid --max-age"},
		{name: "bad max events", args: []string{"srv", "ev", "--max-events", "0"}, wantErr: "invalid --max-events"},
		{name: "missing value", args: []string{"srv", "ev", "--cursor-file"}, wantErr: "--cursor-file requires a value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseEventsArgs(tc.args, tc.watch)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, opts)
		})
	}
}

func TestEventsList_HTTP(t *testing.T) {
	setupTestConfigDir(t)
	invalid := eventDescriptor{
		Name:        "bad.annotation",
		Delivery:    []string{deliveryPoll},
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"number","x-mcp-header":"X"}}}`),
	}
	other := eventDescriptor{Name: "email.received", Description: "New mail", Delivery: []string{deliveryPoll}}
	f := &fakeEventsServer{events: []eventDescriptor{incidentEvent(deliveryPush, deliveryPoll), invalid, other}}
	srv := newFakeEventsServer(t, f)

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents([]string{srv.URL}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []eventOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("output is not a JSON array: %q", stdout)
	}
	if len(got) != 2 || got[0].Name != "email.received" || got[1].Name != "incident.created" {
		t.Fatalf("events = %+v, want email.received and incident.created (sorted, invalid one excluded)", got)
	}
	if got[1].InputSchema != nil || got[1].PayloadSchema != nil {
		t.Error("compact listing should omit schemas")
	}
	if !reflect.DeepEqual(got[1].Delivery, []string{deliveryPush, deliveryPoll}) {
		t.Errorf("delivery = %v", got[1].Delivery)
	}
	if !strings.Contains(stderr, `excluding event "bad.annotation"`) {
		t.Errorf("expected exclusion warning, got %q", stderr)
	}

	stdout = captureStdout(t, func() { err = cmdEvents([]string{srv.URL, "--full", "--query", "INCIDENT"}) })
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	_ = json.Unmarshal([]byte(stdout), &got)
	if len(got) != 1 || got[0].Name != "incident.created" || got[0].InputSchema == nil || got[0].PayloadSchema == nil {
		t.Errorf("--full --query output = %s", stdout)
	}
}

func TestEventsList_AllServersSkipsUnsupported(t *testing.T) {
	setupTestConfigDir(t)
	withEvents := newFakeEventsServer(t, &fakeEventsServer{events: []eventDescriptor{incidentEvent(deliveryPoll)}})
	plainFake := &fakeEventsServer{noExtension: true}
	plain := newFakeEventsServer(t, plainFake)
	for name, url := range map[string]string{"alpha": withEvents.URL, "beta": plain.URL} {
		if err := addServerConfig(ServerConfig{Name: name, Transport: "streamable-http", URL: url}); err != nil {
			t.Fatal(err)
		}
	}

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents(nil) })
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []eventOutput
	_ = json.Unmarshal([]byte(stdout), &got)
	if len(got) != 1 || got[0].Server != "alpha" || got[0].Name != "incident.created" {
		t.Errorf("events = %s, want only alpha's", stdout)
	}
	if strings.Contains(stderr, "warning") {
		t.Errorf("a server without events must be skipped quietly, got %q", stderr)
	}
	if plainFake.sawEventsMethod() {
		t.Error("sent events/* to a server that did not advertise the extension")
	}
}

func TestEvents_UnsupportedServerGetsNoEventsRequests(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{noExtension: true}
	srv := newFakeEventsServer(t, f)

	for _, args := range [][]string{{srv.URL}, {"poll", srv.URL, "incident.created"}, {"watch", srv.URL, "incident.created"}} {
		err := cmdEvents(args)
		var unsupported *eventsUnsupportedError
		if !errors.As(err, &unsupported) {
			t.Errorf("cmdEvents(%v) err = %v, want eventsUnsupportedError", args[0], err)
		}
	}
	if f.sawEventsMethod() {
		t.Error("sent events/* to a server that did not advertise the extension")
	}
}

func TestEventsPoll_DrainsAndPersistsCursor(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPoll, deliveryPush)},
		poll: func(n int, p eventsPollParams) (any, *jsonrpcError) {
			if n == 1 {
				return eventsPollResult{Events: []eventOccurrence{occurrence("e1")}, Cursor: strPtr("c1"), HasMore: true}, nil
			}
			next := int64(5000)
			return eventsPollResult{Events: []eventOccurrence{occurrence("e2")}, Cursor: strPtr("c2"), NextPollMs: &next}, nil
		},
	}
	srv := newFakeEventsServer(t, f)
	cursorFile := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(cursorFile, []byte("c0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	stdout := captureStdout(t, func() {
		err = cmdEvents([]string{"poll", srv.URL, "incident.created",
			"--severity", "P1", "--limit", "5", "--region", "us-west1",
			"--cursor-file", cursorFile, "--max-age", "5m"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out eventsPollResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("output is not JSON: %q", stdout)
	}
	if len(out.Events) != 2 || out.Events[0].EventID != "e1" || out.Events[1].EventID != "e2" {
		t.Errorf("events = %+v, want e1, e2", out.Events)
	}
	if out.Cursor == nil || *out.Cursor != "c2" || out.HasMore || out.NextPollMs == nil || *out.NextPollMs != 5000 {
		t.Errorf("result = %s", stdout)
	}

	polls := f.requestsFor(methodEventsPoll)
	if len(polls) != 2 {
		t.Fatalf("expected 2 polls (hasMore drained), got %d", len(polls))
	}
	var first, second struct {
		eventsPollParams
		Meta map[string]any `json:"_meta"`
	}
	_ = json.Unmarshal(polls[0].Params, &first)
	_ = json.Unmarshal(polls[1].Params, &second)
	wantArgs := map[string]any{"severity": "P1", "limit": float64(5), "region": "us-west1"}
	if !reflect.DeepEqual(first.Arguments, wantArgs) {
		t.Errorf("arguments = %v, want %v (typed from inputSchema)", first.Arguments, wantArgs)
	}
	if first.Cursor == nil || *first.Cursor != "c0" || first.MaxAgeMs != 300000 {
		t.Errorf("first poll cursor/maxAgeMs = %v/%d, want c0/300000", first.Cursor, first.MaxAgeMs)
	}
	if second.Cursor == nil || *second.Cursor != "c1" || second.MaxAgeMs != 0 {
		t.Errorf("second poll cursor/maxAgeMs = %v/%d, want c1/0", second.Cursor, second.MaxAgeMs)
	}
	caps, _ := json.Marshal(first.Meta[metaClientCapabilities])
	if !strings.Contains(string(caps), extensionEvents) {
		t.Errorf("client capabilities %s do not declare the events extension", caps)
	}

	h := polls[0].Header
	if h.Get("Mcp-Method") != methodEventsPoll || h.Get("Mcp-Name") != "incident.created" || h.Get("Mcp-Param-Region") != "us-west1" {
		t.Errorf("headers Mcp-Method=%q Mcp-Name=%q Mcp-Param-Region=%q", h.Get("Mcp-Method"), h.Get("Mcp-Name"), h.Get("Mcp-Param-Region"))
	}

	data, _ := os.ReadFile(cursorFile)
	if string(data) != "c2\n" {
		t.Errorf("cursor file = %q, want c2", data)
	}
}

func TestEventsPoll_StartsFromNowAndCapsEvents(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPoll)},
		poll: func(n int, p eventsPollParams) (any, *jsonrpcError) {
			// Honor maxEvents: two events per page at most.
			evs := []eventOccurrence{occurrence(fmt.Sprintf("e%da", n)), occurrence(fmt.Sprintf("e%db", n))}
			if p.MaxEvents > 0 && p.MaxEvents < len(evs) {
				evs = evs[:p.MaxEvents]
			}
			return eventsPollResult{Events: evs, Cursor: strPtr(fmt.Sprintf("c%d", n)), HasMore: true}, nil
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	stdout := captureStdout(t, func() {
		err = cmdEvents([]string{"poll", srv.URL, "incident.created", "--max-events", "3"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var out eventsPollResult
	_ = json.Unmarshal([]byte(stdout), &out)
	if len(out.Events) != 3 || !out.HasMore || out.Cursor == nil || *out.Cursor != "c2" {
		t.Errorf("result = %s, want 3 events, hasMore, cursor c2", stdout)
	}

	polls := f.requestsFor(methodEventsPoll)
	var first, second map[string]any
	_ = json.Unmarshal(polls[0].Params, &first)
	_ = json.Unmarshal(polls[1].Params, &second)
	if v, ok := first["cursor"]; !ok || v != nil {
		t.Errorf("first poll cursor = %v (present %v), want explicit null", v, ok)
	}
	if args, ok := first["arguments"].(map[string]any); !ok || len(args) != 0 {
		t.Errorf("arguments = %v, want an empty object", first["arguments"])
	}
	if second["maxEvents"] != float64(1) {
		t.Errorf("second poll maxEvents = %v, want the remaining 1", second["maxEvents"])
	}
}

func TestEventsPoll_NullCursorClearsCursorFile(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPoll)},
		poll: func(n int, p eventsPollParams) (any, *jsonrpcError) {
			return eventsPollResult{Events: []eventOccurrence{}}, nil
		},
	}
	srv := newFakeEventsServer(t, f)
	cursorFile := filepath.Join(t.TempDir(), "cursor")
	if err := os.WriteFile(cursorFile, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	captureStdout(t, func() {
		if err := cmdEvents([]string{"poll", srv.URL, "incident.created", "--cursor-file", cursorFile}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(cursorFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cursor file should be removed for an event type without replay (err = %v)", err)
	}
}

func TestEventsPoll_DeliveryModes(t *testing.T) {
	setupTestConfigDir(t)
	cases := []struct {
		delivery []string
		wantErr  string
	}{
		{[]string{deliveryPush}, "use mcp events watch"},
		{[]string{"webhook"}, "webhook is not supported"},
	}
	for _, tc := range cases {
		f := &fakeEventsServer{events: []eventDescriptor{incidentEvent(tc.delivery...)}}
		srv := newFakeEventsServer(t, f)
		err := cmdEvents([]string{"poll", srv.URL, "incident.created"})
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("delivery %v: err = %v, want %q", tc.delivery, err, tc.wantErr)
		}
		if len(f.requestsFor(methodEventsPoll)) != 0 {
			t.Errorf("delivery %v: sent events/poll anyway", tc.delivery)
		}
	}

	f := &fakeEventsServer{events: []eventDescriptor{incidentEvent(deliveryPoll)}}
	srv := newFakeEventsServer(t, f)
	if err := cmdEvents([]string{"poll", srv.URL, "no.such.event"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown event err = %v", err)
	}
}

func TestEventsWatch_HTTP(t *testing.T) {
	setupTestConfigDir(t)
	var gotParams eventsStreamParams
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPush, deliveryPoll)},
		stream: func(n int, p eventsStreamParams, s *sseStream) {
			gotParams = p
			_, _ = fmt.Fprint(s.w, ": comment lines are ignored\n\n")
			s.notify(notifEventsActive, `{"cursor":"c1","truncated":true,"heartbeatMs":15000}`)
			s.notify(notifEventsEvent, `{"eventId":"e1","name":"incident.created","timestamp":"t","data":{"id":"e1"},"cursor":"c2"}`)
			// A redelivery of the same event is dropped.
			s.notify(notifEventsEvent, `{"eventId":"e1","name":"incident.created","timestamp":"t","data":{"id":"e1"},"cursor":"c2"}`)
			// Someone else's subscription is ignored.
			s.send(`{"jsonrpc":"2.0","method":"notifications/events/event","params":{"eventId":"x","name":"incident.created","timestamp":"t","data":{},"_meta":{"io.modelcontextprotocol/subscriptionId":999999}}}`)
			s.notify(notifEventsHeartbeat, `{"cursor":"c3"}`)
			s.notify(notifEventsError, `{"error":{"code":-32603,"message":"UpstreamError"}}`)
			s.close()
		},
	}
	srv := newFakeEventsServer(t, f)
	cursorFile := filepath.Join(t.TempDir(), "cursor")

	var err error
	stdout := captureStdout(t, func() {
		err = cmdEvents([]string{"watch", srv.URL, "incident.created", "--params", `{"severity":"P1","region":"eu"}`, "--cursor-file", cursorFile})
	})
	if err != nil {
		t.Fatal(err)
	}

	lines := decodeEventLines(t, stdout)
	want := []string{"active:c1", "gap:c1", "event:e1", "error", "closed"}
	if got := lineSummary(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %v, want %v\n%s", got, want, stdout)
	}
	if !lines[0].Truncated || lines[0].HeartbeatMs != 15000 {
		t.Errorf("active line = %+v", lines[0])
	}
	if lines[2].Meta != nil || string(lines[2].Data) != `{"id":"e1"}` || *lines[2].Cursor != "c2" {
		t.Errorf("event line = %+v (subscription _meta must be stripped)", lines[2])
	}
	if !strings.Contains(string(lines[3].Error), "UpstreamError") {
		t.Errorf("error line = %s", lines[3].Error)
	}

	if gotParams.Cursor != nil || gotParams.Arguments["severity"] != "P1" {
		t.Errorf("stream params = %+v, want null cursor and severity P1", gotParams)
	}
	h := f.requestsFor(methodEventsStream)[0].Header
	if h.Get("Mcp-Name") != "incident.created" || h.Get("Mcp-Param-Region") != "eu" {
		t.Errorf("headers Mcp-Name=%q Mcp-Param-Region=%q", h.Get("Mcp-Name"), h.Get("Mcp-Param-Region"))
	}

	data, _ := os.ReadFile(cursorFile)
	if string(data) != "c3\n" {
		t.Errorf("cursor file = %q, want the heartbeat's c3", data)
	}
}

func TestEventsWatch_ReconnectsWithLastCursor(t *testing.T) {
	setupTestConfigDir(t)
	slept := stubEventsSleep(t)
	var reconnectCursor *string
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPush)},
		stream: func(n int, p eventsStreamParams, s *sseStream) {
			if n == 1 {
				s.notify(notifEventsActive, `{"cursor":"c1"}`)
				s.notify(notifEventsEvent, `{"eventId":"e1","name":"incident.created","timestamp":"t","data":{},"cursor":"c2"}`)
				return // dropped: no final frame
			}
			reconnectCursor = p.Cursor
			s.notify(notifEventsActive, `{"cursor":"c2"}`)
			s.notify(notifEventsEvent, `{"eventId":"e1","name":"incident.created","timestamp":"t","data":{},"cursor":"c2"}`)
			s.notify(notifEventsEvent, `{"eventId":"e2","name":"incident.created","timestamp":"t","data":{},"cursor":"c3"}`)
			s.close()
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents([]string{"watch", srv.URL, "incident.created"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"active:c1", "event:e1", "active:c2", "event:e2", "closed"}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if reconnectCursor == nil || *reconnectCursor != "c2" {
		t.Errorf("reconnect cursor = %v, want c2", reconnectCursor)
	}
	if len(*slept) != 1 || (*slept)[0] <= 0 || (*slept)[0] > time.Second {
		t.Errorf("reconnect delays = %v, want one jittered delay within 1s", *slept)
	}
	if !strings.Contains(stderr, "reconnecting") {
		t.Errorf("stderr = %q, want a reconnect warning", stderr)
	}
}

func TestEventsWatch_SilentStreamIsAbortedAndReconnected(t *testing.T) {
	setupTestConfigDir(t)
	stubEventsSleep(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPush)},
		stream: func(n int, p eventsStreamParams, s *sseStream) {
			if n == 1 {
				// Grant a 20ms heartbeat, then go silent until the client
				// gives up on the connection.
				s.notify(notifEventsActive, `{"cursor":"c1","heartbeatMs":20}`)
				select {
				case <-s.ctx.Done():
				case <-time.After(5 * time.Second):
					t.Error("client never aborted the silent stream")
				}
				return
			}
			s.notify(notifEventsActive, `{"cursor":"c1"}`)
			s.close()
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents([]string{"watch", srv.URL, "incident.created"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"active:c1", "active:c1", "closed"}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if !strings.Contains(stderr, "no message on the events stream for 60ms") {
		t.Errorf("stderr = %q, want the watchdog warning", stderr)
	}
}

func TestEventsWatch_Terminated(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPush)},
		stream: func(n int, p eventsStreamParams, s *sseStream) {
			s.notify(notifEventsActive, `{"cursor":"c1"}`)
			s.notify(notifEventsTerminated, `{"error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}}}`)
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	stdout := captureStdout(t, func() { err = cmdEvents([]string{"watch", srv.URL, "incident.created"}) })
	if !errors.Is(err, errEventsTerminated) || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err = %v, want errEventsTerminated mentioning Forbidden", err)
	}
	lines := decodeEventLines(t, stdout)
	if got := lineSummary(lines); !reflect.DeepEqual(got, []string{"active:c1", "terminated"}) {
		t.Errorf("lines = %v", got)
	}
	if !strings.Contains(string(lines[1].Error), "Access revoked") {
		t.Errorf("terminated line error = %s", lines[1].Error)
	}
}

func TestEventsWatch_SchemaChangedResubscribes(t *testing.T) {
	setupTestConfigDir(t)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPush)},
		stream: func(n int, p eventsStreamParams, s *sseStream) {
			if n == 1 {
				s.notify(notifEventsActive, `{"cursor":"c1"}`)
				s.notify(notifEventsTerminated, `{"error":{"code":-32026,"message":"Unsupported","data":{"feature":"inputSchema","reason":"schema_changed"}}}`)
				return
			}
			s.notify(notifEventsActive, `{"cursor":"c2"}`)
			s.close()
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	var stdout string
	captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents([]string{"watch", srv.URL, "incident.created"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"active:c1", "terminated", "active:c2", "closed"}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	if n := len(f.requestsFor(methodEventsList)); n != 2 {
		t.Errorf("events/list calls = %d, want 2 (initial + refresh after the schema change)", n)
	}
}

func TestEventsWatch_FallsBackToPolling(t *testing.T) {
	setupTestConfigDir(t)
	slept := stubEventsSleep(t)
	fast, slow := int64(10), int64(60000)
	f := &fakeEventsServer{
		events: []eventDescriptor{incidentEvent(deliveryPoll)},
		poll: func(n int, p eventsPollParams) (any, *jsonrpcError) {
			switch n {
			case 1:
				return eventsPollResult{Events: []eventOccurrence{}, Cursor: strPtr("c1"), Truncated: true, NextPollMs: &fast}, nil
			case 2:
				return eventsPollResult{Events: []eventOccurrence{occurrence("e1")}, Cursor: strPtr("c2"), HasMore: true}, nil
			case 3:
				return eventsPollResult{Events: []eventOccurrence{occurrence("e1"), occurrence("e2")}, Cursor: strPtr("c3"), NextPollMs: &slow}, nil
			}
			return nil, &jsonrpcError{Code: -32023, Message: "NotFound", Data: json.RawMessage(`{"kind":"event"}`)}
		},
	}
	srv := newFakeEventsServer(t, f)

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = cmdEvents([]string{"watch", srv.URL, "incident.created"}) })
	})
	if err == nil || !strings.Contains(err.Error(), "NotFound") || !strings.Contains(err.Error(), `"kind":"event"`) {
		t.Fatalf("err = %v, want the NotFound error with its data", err)
	}
	want := []string{"gap:c1", "event:e1", "event:e2"}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
	// 10ms is below the 1s floor; hasMore skips the wait entirely.
	if wantSleeps := []time.Duration{time.Second, time.Minute}; !reflect.DeepEqual(*slept, wantSleeps) {
		t.Errorf("sleeps = %v, want %v", *slept, wantSleeps)
	}
	if !strings.Contains(stderr, "polling instead") {
		t.Errorf("stderr = %q, want the fallback note", stderr)
	}
	if len(f.requestsFor(methodEventsStream)) != 0 {
		t.Error("opened events/stream for an event without push delivery")
	}
}

func TestEventsFollow_RetriesNetworkErrors(t *testing.T) {
	slept := stubEventsSleep(t)
	calls := 0
	mock := &mockTransport{sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
		calls++
		switch calls {
		case 1:
			return jsonrpcResponse{}, errors.New("connection refused")
		case 2:
			result, _ := json.Marshal(eventsPollResult{Events: []eventOccurrence{occurrence("e1")}, Cursor: strPtr("c1")})
			return jsonrpcResponse{JSONRPC: "2.0", Result: result}, nil
		}
		return jsonrpcResponse{JSONRPC: "2.0", Error: &jsonrpcError{Code: -32024, Message: "Forbidden"}}, nil
	}}
	sub := &eventSubscription{
		transport: newProtocolSession(mock, protocolVersionModern),
		desc:      incidentEvent(deliveryPoll),
		args:      map[string]any{},
		seen:      newRecentIDs(recentEventIDs),
	}

	var err error
	var stdout string
	captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = sub.follow(time.Second, 0) })
	})
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("err = %v, want Forbidden to end the loop", err)
	}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, []string{"event:e1"}) {
		t.Errorf("lines = %v", got)
	}
	// One backoff for the network error, then the default interval.
	if len(*slept) != 2 || (*slept)[0] > time.Second || (*slept)[1] != defaultPollInterval {
		t.Errorf("sleeps = %v", *slept)
	}
}

func TestEventsWatch_StdioCancelsAfterTermination(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()
	transport := newProtocolSession(newTestStdioTransport(clientStdinWriter, clientStdoutReader), protocolVersionModern)

	cancelled := make(chan float64, 1)
	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var msg struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.Unmarshal(scanner.Bytes(), &msg)
			switch msg.Method {
			case methodEventsStream:
				meta := fmt.Sprintf(`{"io.modelcontextprotocol/subscriptionId":%d}`, msg.ID)
				_, _ = fmt.Fprintf(serverStdoutWriter, `{"jsonrpc":"2.0","method":"notifications/events/active","params":{"cursor":null,"_meta":%s}}`+"\n", meta)
				_, _ = fmt.Fprintf(serverStdoutWriter, `{"jsonrpc":"2.0","method":"notifications/events/terminated","params":{"error":{"code":-32023,"message":"NotFound"},"_meta":%s}}`+"\n", meta)
				// No final frame: a stdio server may omit it.
			case "notifications/cancelled":
				id, _ := msg.Params["requestId"].(float64)
				cancelled <- id
				return
			}
		}
	}()

	sub := &eventSubscription{
		transport: transport,
		desc:      incidentEvent(deliveryPush),
		args:      map[string]any{},
		seen:      newRecentIDs(recentEventIDs),
	}
	done := make(chan error, 1)
	var stdout string
	go func() {
		var err error
		stdout = captureStdout(t, func() { err = sub.watch() })
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, errEventsTerminated) {
			t.Fatalf("err = %v, want errEventsTerminated", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not return after termination on stdio")
	}
	select {
	case id := <-cancelled:
		if id == 0 {
			t.Error("notifications/cancelled carried no requestId")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stdio server never got notifications/cancelled")
	}
	if got := lineSummary(decodeEventLines(t, stdout)); !reflect.DeepEqual(got, []string{"active", "terminated"}) {
		t.Errorf("lines = %v", got)
	}
}

func TestStdioTransport_StreamingContextCancel(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()
	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)
	transport.callTimeout = time.Millisecond // must not apply to streams

	go func() {
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			// A notification, then silence: no response ever.
			_, _ = fmt.Fprintln(serverStdoutWriter, `{"jsonrpc":"2.0","method":"notifications/events/heartbeat","params":{}}`)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	var got int
	_, err := transport.sendStreamingContext(ctx, jsonrpcRequest{JSONRPC: "2.0", ID: 7, Method: methodEventsStream}, func(evt streamEvent) {
		got++
		time.Sleep(20 * time.Millisecond) // outlive the call timeout
		cancel()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got != 1 {
		t.Errorf("notifications delivered = %d, want 1", got)
	}
	transport.mu.Lock()
	pending := len(transport.pending)
	transport.mu.Unlock()
	if pending != 0 {
		t.Errorf("pending requests = %d after cancel, want 0", pending)
	}
}

func TestRecentIDs(t *testing.T) {
	r := newRecentIDs(2)
	for _, id := range []string{"a", "b"} {
		if r.add(id) {
			t.Errorf("%s reported as seen on first add", id)
		}
	}
	if !r.add("a") {
		t.Error("a not reported as seen")
	}
	r.add("c") // evicts a
	if r.add("a") {
		t.Error("a should have been evicted")
	}
	if r.add("") {
		t.Error("empty IDs are never duplicates")
	}
}
