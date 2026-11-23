package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const eventsUsage = `Usage: mcp events [server|url] [--query <q>] [--full]
       mcp events poll <server|url> <event> [flags]
       mcp events watch <server|url> <event> [flags]

Events extension (SEP-3415): list a server's event types, fetch new
occurrences, or stream them as they happen. Needs a server that advertises
io.modelcontextprotocol/events (protocol 2026-07-28 or later).

List flags:
  --query <q>            Filter by name/description
  --full                 Include inputSchema and payloadSchema

Subscription arguments (the event's inputSchema; see --full):
  --<arg> <value>        One argument, typed from the inputSchema
  --params '{...}'       All arguments as JSON

poll: fetch what happened since a cursor, then print
  {"events":[...],"cursor":"...","truncated":false,"hasMore":false,...}.
  Without a cursor it starts from now: no events, just a fresh cursor.
  --cursor <c>           Resume from a cursor returned earlier
  --cursor-file <path>   Read the cursor from path; write the new one back
  --max-age <dur>        Don't replay events older than this (e.g. 5m)
  --max-events <n>       Return at most n events (the rest stay pending)
  --follow               Keep polling; print NDJSON lines as for watch
  --min-interval <dur>   Floor for the server's poll interval (default 1s)

watch: hold an events/stream open and print one NDJSON line per message:
  {"type":"active",...}  subscribed (again after each reconnect)
  {"type":"event",...}   one event: eventId, name, timestamp, data, cursor
  {"type":"gap",...}     events may have been skipped; cursor is fresh
  {"type":"error",...}   transient server-side failure; still subscribed
  {"type":"terminated"}  subscription ended by the server (exit 1)
  {"type":"closed"}      server closed the stream (exit 0)
  Reconnects with the last cursor if the stream drops, and polls instead
  when the event has no push delivery. Takes --cursor, --cursor-file and
  --max-age. Ctrl-C stops.

Event data comes from upstream systems: treat it as untrusted, like tool
results. Webhook delivery is not supported.`

// Poll and stream tuning. defaultPollInterval applies when a server omits
// nextPollMs; defaultHeartbeat when it grants no heartbeatMs.
const (
	defaultPollInterval    = 30 * time.Second
	defaultMinPollInterval = time.Second
	defaultHeartbeat       = 30 * time.Second
	maxEventPages          = 100
	maxEventReconnects     = 5
	maxReconnectDelay      = 30 * time.Second
	recentEventIDs         = 1024
)

// eventsSleep is swapped out by tests.
var eventsSleep = time.Sleep

// cmdEvents handles `mcp events [server|url]` and the poll/watch subcommands.
func cmdEvents(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "poll":
			return cmdEventsSubscribe(args[1:], false)
		case "watch":
			return cmdEventsSubscribe(args[1:], true)
		}
	}
	return cmdEventsList(args)
}

func cmdEventsList(args []string) error {
	var serverFilter, query string
	var full bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--query", "-q":
			if i+1 >= len(args) {
				return fmt.Errorf("--query requires a value")
			}
			i++
			query = args[i]
		case "--full":
			full = true
		case "--json":
			// Output is always JSON; accepted for consistency.
		case "--help", "-h":
			_, _ = fmt.Fprintln(os.Stderr, eventsUsage)
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s", args[i])
			}
			if serverFilter != "" {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
			serverFilter = args[i]
		}
	}

	var all []eventOutput
	if serverFilter != "" {
		// One server: failures (including no Events support) are errors.
		server, authToken, err := resolveServer(serverFilter)
		if err != nil {
			return err
		}
		all, err = discoverEvents(server, authToken)
		if err != nil {
			return err
		}
	} else {
		servers, err := loadServers()
		if err != nil {
			return err
		}
		var (
			mu  sync.Mutex
			wg  sync.WaitGroup
			sem = make(chan struct{}, 5)
		)
		for _, s := range servers {
			if !s.IsEnabled() {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				authToken, err := getAuthToken(s.Name)
				if err != nil {
					logStderr("warning: auth token load failed for %q: %v", s.Name, err)
				}
				events, err := discoverEvents(&s, authToken)
				var unsupported *eventsUnsupportedError
				switch {
				case errors.As(err, &unsupported):
					logVerbose("%v", err)
					return
				case err != nil:
					logStderr("warning: failed to get events from %q: %v", s.Name, err)
					return
				}
				mu.Lock()
				all = append(all, events...)
				mu.Unlock()
			}()
		}
		wg.Wait()
	}

	return outputEventsList(all, query, full)
}

func outputEventsList(events []eventOutput, query string, full bool) error {
	sort.Slice(events, func(i, j int) bool {
		if events[i].Server != events[j].Server {
			return events[i].Server < events[j].Server
		}
		return events[i].Name < events[j].Name
	})
	q := strings.ToLower(query)
	out := []eventOutput{}
	for _, e := range events {
		if q != "" && !strings.Contains(strings.ToLower(e.Name), q) && !strings.Contains(strings.ToLower(e.Description), q) {
			continue
		}
		if !full {
			e.InputSchema, e.PayloadSchema = nil, nil
		}
		out = append(out, e)
	}
	return outputJSON(out)
}

// discoverEvents connects to a server and lists its event types.
func discoverEvents(server *ServerConfig, authToken string) ([]eventOutput, error) {
	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transport.Close() }()

	if err := requireEvents(transport, server.Name); err != nil {
		return nil, err
	}
	descs, err := listAllEvents(transport)
	if err != nil {
		return nil, err
	}
	out := make([]eventOutput, 0, len(descs))
	for _, d := range descs {
		out = append(out, eventOutput{
			Server:        server.Name,
			Name:          d.Name,
			Description:   d.Description,
			Delivery:      d.Delivery,
			InputSchema:   d.InputSchema,
			PayloadSchema: d.PayloadSchema,
		})
	}
	return out, nil
}

// eventsUnsupportedError reports a server that does not advertise the
// Events extension. Clients must not send it events/* requests.
type eventsUnsupportedError struct {
	server string
}

func (e *eventsUnsupportedError) Error() string {
	return fmt.Sprintf("server %q does not support events (no %s extension in server/discover; needs protocol %s or later)", e.server, extensionEvents, protocolVersionModern)
}

// requireEvents fails unless the server behind transport advertised the
// Events extension in server/discover. A direct modern session kept the
// discover result from negotiation; a daemon connection hides it, so the
// daemon forwards a fresh server/discover. Legacy servers (no
// server/discover) never support it.
func requireEvents(transport Transport, serverName string) error {
	var caps json.RawMessage
	switch t := transport.(type) {
	case *protocolSession:
		caps = t.capabilities
	case *DaemonTransport:
		resp, err := t.Send(jsonrpcRequest{JSONRPC: jsonrpcVersion, ID: nextID(), Method: "server/discover"})
		if err != nil {
			return fmt.Errorf("server/discover: %w", err)
		}
		var result discoverResult
		if resp.Error == nil && json.Unmarshal(resp.Result, &result) == nil {
			caps = result.Capabilities
		}
	}
	var parsed struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if len(caps) > 0 && json.Unmarshal(caps, &parsed) == nil {
		if _, ok := parsed.Extensions[extensionEvents]; ok {
			return nil
		}
	}
	return &eventsUnsupportedError{server: serverName}
}

// listAllEvents fetches every event type, following pagination. On modern
// streamable HTTP, event types with invalid x-mcp-header annotations must be
// excluded, as for tools.
func listAllEvents(transport Transport) ([]eventDescriptor, error) {
	var all []eventDescriptor
	var cursor string
	modernHTTP := isModernHTTP(transport)

	for page := 0; page < maxEventPages; page++ {
		var params any
		if cursor != "" {
			params = eventsListParams{Cursor: cursor}
		}
		resp, err := transport.Send(jsonrpcRequest{
			JSONRPC: jsonrpcVersion,
			ID:      nextID(),
			Method:  methodEventsList,
			Params:  params,
		})
		if err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		if resp.Error != nil {
			return nil, eventsError(methodEventsList, resp.Error)
		}
		var result eventsListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("unmarshal events: %w", err)
		}
		for _, d := range result.Events {
			if modernHTTP {
				if _, err := extractHeaderParams(d.InputSchema); err != nil {
					logStderr("warning: excluding event %q: invalid x-mcp-header annotation: %v", d.Name, err)
					continue
				}
			}
			all = append(all, d)
		}
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}

	logStderr("warning: events list truncated after %d pages", maxEventPages)
	return all, nil
}

// eventsError renders a JSON-RPC error from an events/* method, keeping its
// typed data (e.g. {"feature":"deliveryMode"}) visible. The *jsonrpcError
// stays reachable through errors.As.
func eventsError(method string, e *jsonrpcError) error {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Errorf("%s: %w %s", method, e, e.Data)
	}
	return fmt.Errorf("%s: %w", method, e)
}

// eventsOptions holds the parsed flags of `mcp events poll|watch`.
type eventsOptions struct {
	server, event string
	paramsJSON    string
	argFlags      map[string]string
	cursor        *string
	cursorFile    string
	maxAge        time.Duration
	maxEvents     int
	follow        bool
	minInterval   time.Duration
	help          bool
}

// parseEventsArgs parses `mcp events poll|watch` arguments. Flags it does
// not know are subscription arguments, as with `mcp call`.
func parseEventsArgs(args []string, watch bool) (eventsOptions, error) {
	opts := eventsOptions{argFlags: map[string]string{}, minInterval: defaultMinPollInterval}
	var positionals []string
	var pollOnly []string

	value := func(i int, flag string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return args[i+1], nil
	}
	duration := func(i int, flag string) (time.Duration, error) {
		v, err := value(i, flag)
		if err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("invalid %s value %q (want a duration like 30s or 5m)", flag, v)
		}
		return d, nil
	}

	for i := 0; i < len(args); i++ {
		var err error
		switch a := args[i]; a {
		case "--help", "-h":
			opts.help = true
		case "--params", "-p":
			opts.paramsJSON, err = value(i, a)
			i++
		case "--cursor":
			var c string
			if c, err = value(i, a); err == nil && c != "" {
				opts.cursor = &c
			}
			i++
		case "--cursor-file":
			opts.cursorFile, err = value(i, a)
			i++
		case "--max-age":
			opts.maxAge, err = duration(i, a)
			i++
		case "--max-events":
			var v string
			if v, err = value(i, a); err == nil {
				opts.maxEvents, err = strconv.Atoi(v)
				if err != nil || opts.maxEvents < 1 {
					err = fmt.Errorf("invalid --max-events value %q (want a positive integer)", v)
				}
			}
			pollOnly = append(pollOnly, a)
			i++
		case "--follow":
			opts.follow = true
			pollOnly = append(pollOnly, a)
		case "--min-interval":
			opts.minInterval, err = duration(i, a)
			pollOnly = append(pollOnly, a)
			i++
		default:
			if !strings.HasPrefix(a, "--") {
				positionals = append(positionals, a)
				continue
			}
			key := strings.TrimPrefix(a, "--")
			if k, v, ok := strings.Cut(key, "="); ok {
				opts.argFlags[k] = v
			} else if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				opts.argFlags[key] = "true"
			} else {
				i++
				opts.argFlags[key] = args[i]
			}
		}
		if err != nil {
			return opts, err
		}
	}

	if opts.help {
		return opts, nil
	}
	mode := "poll"
	if watch {
		mode = "watch"
		if len(pollOnly) > 0 {
			return opts, fmt.Errorf("%s applies to mcp events poll, not watch", pollOnly[0])
		}
	}
	if len(positionals) != 2 {
		return opts, fmt.Errorf("usage: mcp events %s <server|url> <event> [--<arg> <value> ...] [--params '{...}']", mode)
	}
	if opts.paramsJSON != "" && len(opts.argFlags) > 0 {
		return opts, fmt.Errorf("cannot combine --params with individual argument flags")
	}
	opts.server, opts.event = positionals[0], positionals[1]
	return opts, nil
}

// arguments builds the subscription arguments from --params or the
// individual flags, typed from the event's inputSchema. Always non-nil: the
// request carries an arguments object even when it is empty.
func (o eventsOptions) arguments(desc eventDescriptor) (map[string]any, error) {
	args := map[string]any{}
	if o.paramsJSON != "" {
		if err := json.Unmarshal([]byte(o.paramsJSON), &args); err != nil {
			return nil, fmt.Errorf("invalid params JSON: %w", err)
		}
		if args == nil {
			args = map[string]any{}
		}
		return args, nil
	}
	if len(o.argFlags) == 0 {
		return args, nil
	}
	params, complexTypes := parseInputSchema(desc.InputSchema)
	return coerceDynamicFlags(o.argFlags, params, complexTypes)
}

// cmdEventsSubscribe handles `mcp events poll` and `mcp events watch`.
func cmdEventsSubscribe(args []string, watch bool) error {
	opts, err := parseEventsArgs(args, watch)
	if err != nil {
		return err
	}
	if opts.help {
		_, _ = fmt.Fprintln(os.Stderr, eventsUsage)
		return nil
	}

	server, authToken, err := resolveServer(opts.server)
	if err != nil {
		return err
	}

	// A one-shot poll may go through the daemon (which keeps a stdio
	// server's in-memory event buffer alive between invocations). Long-lived
	// loops connect directly: the daemon serves one client per server at a
	// time, so holding it would starve every other client.
	var transport Transport
	if watch || opts.follow {
		transport, err = mcpConnectDirect(server, authToken)
	} else {
		transport, err = mcpConnect(server, authToken)
	}
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	sub, err := openEventSubscription(transport, server.Name, opts)
	if err != nil {
		return err
	}

	switch {
	case watch && sub.offers(deliveryPush):
		return sub.watch()
	case !sub.offers(deliveryPoll) && sub.offers(deliveryPush):
		return fmt.Errorf("event %q offers push delivery only; use mcp events watch", sub.desc.Name)
	case !sub.offers(deliveryPoll):
		return fmt.Errorf("event %q offers no delivery mode mcp supports (delivery: %s; webhook is not supported)", sub.desc.Name, strings.Join(sub.desc.Delivery, ", "))
	case watch:
		logStderr("note: event %q has no push delivery; polling instead", sub.desc.Name)
		return sub.follow(opts.minInterval, 0)
	case opts.follow:
		return sub.follow(opts.minInterval, opts.maxEvents)
	default:
		return sub.pollOnce(opts.maxEvents)
	}
}

// eventSubscription is one (event, arguments) subscription on a connected
// server, with the client-owned state the spec puts on the client: the
// cursor and recently seen event IDs.
type eventSubscription struct {
	transport  Transport
	serverName string
	desc       eventDescriptor
	args       map[string]any
	// headers are the Mcp-Param-* headers mirrored from x-mcp-header
	// annotated arguments (modern streamable HTTP only).
	headers map[string]string
	// maxAgeMs bounds replay from a stale cursor. A poll loop sends it on its
	// first request only: on later polls the cursor is fresh, and a floor
	// shorter than the poll interval would silently drop events.
	maxAgeMs int64

	// mu guards the fields below: stdio delivers stream notifications on
	// the transport's reader goroutine.
	mu         sync.Mutex
	cursor     *string
	cursorFile string
	persisted  *string // cursor last read from or written to cursorFile
	seen       recentIDs
}

func openEventSubscription(transport Transport, serverName string, opts eventsOptions) (*eventSubscription, error) {
	if err := requireEvents(transport, serverName); err != nil {
		return nil, err
	}
	desc, err := findEvent(transport, serverName, opts.event)
	if err != nil {
		return nil, err
	}
	args, err := opts.arguments(desc)
	if err != nil {
		return nil, err
	}

	s := &eventSubscription{
		transport:  transport,
		serverName: serverName,
		desc:       desc,
		args:       args,
		maxAgeMs:   opts.maxAge.Milliseconds(),
		cursorFile: opts.cursorFile,
		seen:       newRecentIDs(recentEventIDs),
	}
	if opts.cursorFile != "" {
		c, err := readCursorFile(opts.cursorFile)
		if err != nil {
			return nil, err
		}
		s.cursor, s.persisted = c, c
	}
	if opts.cursor != nil {
		s.cursor = opts.cursor
	}
	s.updateHeaders()
	return s, nil
}

func findEvent(transport Transport, serverName, name string) (eventDescriptor, error) {
	descs, err := listAllEvents(transport)
	if err != nil {
		return eventDescriptor{}, err
	}
	for _, d := range descs {
		if d.Name == name {
			return d, nil
		}
	}
	return eventDescriptor{}, fmt.Errorf("event %q not found on %s (list them with: mcp events %s)", name, serverName, serverName)
}

func (s *eventSubscription) offers(mode string) bool {
	// A descriptor without delivery modes is non-conformant; let the server
	// decide rather than refusing up front.
	return len(s.desc.Delivery) == 0 || slices.Contains(s.desc.Delivery, mode)
}

func (s *eventSubscription) updateHeaders() {
	s.headers = nil
	if !isModernHTTP(s.transport) {
		return
	}
	// Annotations were validated by listAllEvents.
	if params, err := extractHeaderParams(s.desc.InputSchema); err == nil {
		s.headers = headerValuesForCall(params, s.args)
	}
}

// refresh re-reads the event's descriptor after a HeaderMismatch or an
// in-place schema change, and recomputes the Mcp-Param-* headers.
func (s *eventSubscription) refresh() error {
	logVerbose("refreshing event %q from events/list", s.desc.Name)
	desc, err := findEvent(s.transport, s.serverName, s.desc.Name)
	if err != nil {
		return err
	}
	s.desc = desc
	s.updateHeaders()
	return nil
}

// readCursorFile returns the cursor saved in path, or nil (start from now)
// when the file is missing or empty.
func readCursorFile(path string) (*string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cursor file: %w", err)
	}
	c := strings.TrimSpace(string(data))
	if c == "" {
		return nil, nil
	}
	return &c, nil
}

// persistCursor writes the current cursor to the cursor file when it has
// changed. A null cursor means the event type has no replay, so there is
// nothing to resume from: the file is removed. Caller holds s.mu.
func (s *eventSubscription) persistCursor() {
	if s.cursorFile == "" || equalCursor(s.cursor, s.persisted) {
		return
	}
	var err error
	if s.cursor == nil {
		if err = os.Remove(s.cursorFile); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = atomicWrite(s.cursorFile, []byte(*s.cursor+"\n"))
	}
	if err != nil {
		logStderr("warning: cursor file not updated: %v", err)
		return
	}
	s.persisted = s.cursor
}

func equalCursor(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// poll sends one events/poll. A HeaderMismatch refreshes the descriptor and
// retries once.
func (s *eventSubscription) poll(maxEvents int) (eventsPollResult, error) {
	s.mu.Lock()
	params := eventsPollParams{
		Name:      s.desc.Name,
		Arguments: s.args,
		Cursor:    s.cursor,
		MaxAgeMs:  s.maxAgeMs,
		MaxEvents: maxEvents,
	}
	s.maxAgeMs = 0
	s.mu.Unlock()

	var resp jsonrpcResponse
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = s.transport.Send(jsonrpcRequest{
			JSONRPC:      jsonrpcVersion,
			ID:           nextID(),
			Method:       methodEventsPoll,
			Params:       params,
			ExtraHeaders: s.headers,
		})
		mismatch := isHeaderMismatch(err) || (err == nil && resp.Error != nil && resp.Error.Code == codeHeaderMismatch)
		if !mismatch || attempt > 0 {
			break
		}
		if rerr := s.refresh(); rerr != nil {
			return eventsPollResult{}, rerr
		}
	}
	if err != nil {
		return eventsPollResult{}, fmt.Errorf("%s: %w", methodEventsPoll, err)
	}
	if resp.Error != nil {
		return eventsPollResult{}, eventsError(methodEventsPoll, resp.Error)
	}
	var result eventsPollResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return eventsPollResult{}, fmt.Errorf("unmarshal poll result: %w", err)
	}
	return result, nil
}

// pollOnce polls until the backlog is drained (hasMore false) or maxEvents
// (0 = no cap) events are in hand, prints the combined result, and only
// then persists the cursor, so a failed write never skips events.
func (s *eventSubscription) pollOnce(maxEvents int) error {
	out := eventsPollResult{Events: []eventOccurrence{}}
	for page := 0; page < maxEventPages; page++ {
		limit := 0
		if maxEvents > 0 {
			limit = maxEvents - len(out.Events)
		}
		result, err := s.poll(limit)
		if err != nil {
			return err
		}
		out.Events = append(out.Events, result.Events...)
		out.Cursor = result.Cursor
		out.Truncated = out.Truncated || result.Truncated
		out.HasMore = result.HasMore
		out.NextPollMs = result.NextPollMs
		s.mu.Lock()
		s.cursor = result.Cursor
		s.mu.Unlock()
		if !result.HasMore || (maxEvents > 0 && len(out.Events) >= maxEvents) {
			break
		}
	}
	if err := outputJSON(out); err != nil {
		return err
	}
	s.mu.Lock()
	s.persistCursor()
	s.mu.Unlock()
	return nil
}

// follow polls forever at the server's recommended interval (never faster
// than minInterval), printing NDJSON lines. Network failures are retried
// with backoff; a JSON-RPC error ends the subscription, as the spec defines
// termination for poll.
func (s *eventSubscription) follow(minInterval time.Duration, maxEvents int) error {
	failures := 0
	for {
		result, err := s.poll(maxEvents)
		if err != nil {
			if !isTransientEventsError(err) {
				return err
			}
			failures++
			if failures > maxEventReconnects {
				return fmt.Errorf("giving up after %d failed polls: %w", failures, err)
			}
			delay := reconnectDelay(failures)
			logStderr("warning: %v; retrying in %s", err, delay.Round(time.Millisecond))
			eventsSleep(delay)
			continue
		}
		failures = 0

		s.mu.Lock()
		// Without replay (null cursor) there is no position to have
		// skipped past, so the spec says to ignore truncated.
		if result.Truncated && result.Cursor != nil {
			printEventsLine(eventsLine{Type: "gap", Cursor: result.Cursor})
		}
		for _, occ := range result.Events {
			s.printEvent(occ)
		}
		s.cursor = result.Cursor
		s.persistCursor()
		s.mu.Unlock()

		if result.HasMore {
			continue
		}
		interval := defaultPollInterval
		if result.NextPollMs != nil {
			interval = time.Duration(*result.NextPollMs) * time.Millisecond
		}
		eventsSleep(max(interval, minInterval))
	}
}

// isTransientEventsError reports whether a failed request is worth
// retrying: a network-level failure, not a server's JSON-RPC or HTTP-status
// answer.
func isTransientEventsError(err error) bool {
	var rpcErr *jsonrpcError
	var statusErr *httpStatusError
	return !errors.As(err, &rpcErr) && !errors.As(err, &statusErr)
}

// reconnectDelay is exponential backoff (1s, 2s, 4s… capped) with random
// jitter, so a server restart does not bring every client back at once.
func reconnectDelay(attempt int) time.Duration {
	d := min(time.Second<<(attempt-1), maxReconnectDelay)
	return d/2 + rand.N(d/2+1)
}

// printEvent prints one event unless its eventId was delivered recently
// (redeliveries are expected around reconnects). Caller holds s.mu.
func (s *eventSubscription) printEvent(occ eventOccurrence) {
	if s.seen.add(occ.EventID) {
		logVerbose("skipping duplicate event %q", occ.EventID)
		return
	}
	printEventsLine(eventLine{Type: "event", eventOccurrence: occ})
}

func printEventsLine(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		logStderr("warning: marshal events line: %v", err)
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}

// streamOutcome is how one events/stream request ended, short of a fatal
// error.
type streamOutcome int

const (
	streamClosed      streamOutcome = iota // server closed the stream gracefully
	streamDropped                          // connection lost or went silent: reconnect
	streamResubscribe                      // schema changed in place: re-list, resubscribe
)

// errEventsTerminated marks a subscription the server ended; the
// terminated line has already been printed.
var errEventsTerminated = errors.New("subscription terminated by the server")

// watch holds an events/stream open, reconnecting with the last cursor when
// it drops, until the server closes or terminates the subscription.
func (s *eventSubscription) watch() error {
	var currentID atomic.Int64
	cancelOnInterrupt(s.transport, func() int { return int(currentID.Load()) })

	failures := 0
	for {
		id := nextID()
		currentID.Store(int64(id))
		outcome, activated, err := s.streamOnce(id)
		if err != nil {
			return err
		}
		if outcome == streamClosed {
			printEventsLine(eventsLine{Type: "closed"})
			return nil
		}
		// A stream the server confirmed resets the budget, so only
		// back-to-back failures (or resubscribes) count toward giving up.
		if activated {
			failures = 0
		}
		failures++
		if failures > maxEventReconnects {
			return fmt.Errorf("events stream failed %d times in a row; giving up", failures)
		}
		if outcome == streamResubscribe {
			if err := s.refresh(); err != nil {
				return err
			}
			continue
		}
		delay := reconnectDelay(failures)
		logStderr("warning: events stream dropped; reconnecting in %s", delay.Round(time.Millisecond))
		eventsSleep(delay)
	}
}

// streamOnce runs one events/stream request. activated reports whether the
// server confirmed the subscription, which resets the reconnect budget.
func (s *eventSubscription) streamOnce(reqID int) (outcome streamOutcome, activated bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdio := isStdioSession(s.transport)
	var (
		stalled    atomic.Bool
		heartbeat  = defaultHeartbeat
		terminated *jsonrpcError
		watchdog   *time.Timer
	)
	// On HTTP a stream that stops sending (not even heartbeats) for three
	// heartbeat intervals is dead: abort and reconnect. Stdio detects a dead
	// server by EOF instead.
	if !stdio {
		watchdog = time.AfterFunc(3*heartbeat, func() {
			stalled.Store(true)
			cancel()
		})
		defer watchdog.Stop()
	}

	s.mu.Lock()
	params := eventsStreamParams{Name: s.desc.Name, Arguments: s.args, Cursor: s.cursor, MaxAgeMs: s.maxAgeMs}
	s.mu.Unlock()

	onEvent := func(evt streamEvent) {
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &msg); err != nil || msg.Method == "" {
			logVerbose("skipping non-notification stream event")
			return
		}
		var envelope struct {
			Meta map[string]any `json:"_meta"`
		}
		_ = json.Unmarshal(msg.Params, &envelope)
		if !subscriptionIDMatches(envelope.Meta[metaSubscriptionID], reqID) {
			logVerbose("skipping %s for a different subscription", msg.Method)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		switch msg.Method {
		case notifEventsActive:
			var p struct {
				Cursor      *string `json:"cursor"`
				Truncated   bool    `json:"truncated"`
				HeartbeatMs int64   `json:"heartbeatMs"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			activated = true
			if p.HeartbeatMs > 0 {
				heartbeat = time.Duration(p.HeartbeatMs) * time.Millisecond
			}
			truncated := p.Truncated && p.Cursor != nil // ignored without replay
			printEventsLine(eventsLine{Type: "active", Cursor: p.Cursor, Truncated: truncated, HeartbeatMs: heartbeat.Milliseconds()})
			if truncated {
				printEventsLine(eventsLine{Type: "gap", Cursor: p.Cursor})
			}
			s.cursor = p.Cursor
		case notifEventsEvent:
			var occ eventOccurrence
			if err := json.Unmarshal(msg.Params, &occ); err != nil {
				logStderr("warning: skipping malformed event: %v", err)
				return
			}
			delete(occ.Meta, metaSubscriptionID)
			if len(occ.Meta) == 0 {
				occ.Meta = nil
			}
			s.printEvent(occ)
			s.cursor = occ.Cursor
		case notifEventsHeartbeat:
			var p struct {
				Cursor *string `json:"cursor"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.cursor = p.Cursor
		case notifEventsError, notifEventsTerminated:
			var p struct {
				Error json.RawMessage `json:"error"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if msg.Method == notifEventsError {
				printEventsLine(eventsLine{Type: "error", Error: p.Error})
				break
			}
			printEventsLine(eventsLine{Type: "terminated", Error: p.Error})
			terminated = &jsonrpcError{}
			_ = json.Unmarshal(p.Error, terminated)
			cancel() // the subscription is over; stop waiting for the close
		default:
			logVerbose("skipping unexpected %s on the events stream", msg.Method)
			return
		}
		s.persistCursor()
		if watchdog != nil {
			watchdog.Reset(3 * heartbeat)
		}
	}

	req := jsonrpcRequest{
		JSONRPC:      jsonrpcVersion,
		ID:           reqID,
		Method:       methodEventsStream,
		Params:       params,
		ExtraHeaders: s.headers,
	}
	var resp jsonrpcResponse
	if session, ok := s.transport.(*protocolSession); ok {
		resp, err = session.sendStreamingContext(ctx, req, onEvent)
	} else {
		resp, err = s.transport.SendStreaming(req, onEvent)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if terminated != nil {
		if stdio {
			s.notifyCancelled(reqID)
		}
		if terminated.Code == codeUnsupported && strings.Contains(string(terminated.Data), "schema_changed") {
			logStderr("note: event %q changed incompatibly; resubscribing", s.desc.Name)
			return streamResubscribe, activated, nil
		}
		return 0, activated, fmt.Errorf("%w: %s", errEventsTerminated, terminated.Message)
	}
	switch {
	case err == nil && resp.Error != nil && resp.Error.Code == codeHeaderMismatch,
		err != nil && isHeaderMismatch(err):
		// The descriptor changed under us; resubscribing re-lists it.
		return streamResubscribe, activated, nil
	case err == nil && resp.Error != nil:
		return 0, activated, eventsError(methodEventsStream, resp.Error)
	case err == nil:
		return streamClosed, activated, nil
	case stalled.Load():
		logStderr("warning: no message on the events stream for %s", 3*heartbeat)
		return streamDropped, activated, nil
	case stdio:
		// The child process is gone; there is nothing to reconnect to.
		return 0, activated, fmt.Errorf("events stream closed unexpectedly: %w", err)
	case !isTransientEventsError(err):
		return 0, activated, fmt.Errorf("%s: %w", methodEventsStream, err)
	default:
		return streamDropped, activated, nil
	}
}

// notifyCancelled tells a stdio server to stop an events/stream (stdio has no
// connection to close).
func (s *eventSubscription) notifyCancelled(reqID int) {
	_ = s.transport.Notify(jsonrpcNotification{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/cancelled",
		Params:  map[string]any{"requestId": reqID},
	})
}

// recentIDs remembers the most recent event IDs, for de-duplicating
// redeliveries within a subscription.
type recentIDs struct {
	set   map[string]bool
	order []string
	limit int
}

func newRecentIDs(limit int) recentIDs {
	return recentIDs{set: make(map[string]bool), limit: limit}
}

// add records id and reports whether it was already recorded.
func (r *recentIDs) add(id string) bool {
	if id == "" {
		return false
	}
	if r.set[id] {
		return true
	}
	r.set[id] = true
	r.order = append(r.order, id)
	if len(r.order) > r.limit {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
	return false
}
