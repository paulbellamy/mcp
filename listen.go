package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
)

// cmdListen handles the `mcp listen` command: a long-lived
// subscriptions/listen request whose response stream carries opted-in change
// notifications (2026-07-28; replaces the removed HTTP GET stream).
func cmdListen(args []string) error {
	serverName, params, showHelp, err := parseListenArgs(args)
	if err != nil {
		return err
	}
	if showHelp {
		_, _ = fmt.Fprintln(os.Stderr, `Usage: mcp listen <server|url> [--tools] [--prompts] [--resources] [--resource <uri> ...] [--json]

Hold a subscriptions/listen stream open and print one JSON line per change
notification. Requires a server speaking protocol 2026-07-28 or later; at
least one filter flag is required. Ctrl-C closes the stream (the spec's
cancellation signal).

Flags:
  --tools             Subscribe to tools list changes
  --prompts           Subscribe to prompts list changes
  --resources         Subscribe to resources list changes
  --resource <uri>    Subscribe to updates of one resource (repeatable)
  --json              Output as JSON lines (always on; accepted for consistency)`)
		return nil
	}

	server, authToken, err := resolveServer(serverName)
	if err != nil {
		return err
	}

	// Connect directly, never through the daemon socket: the daemon
	// serializes one client per server, and a long-lived listen stream
	// would starve every other client of that server.
	transport, err := mcpConnectDirect(server, authToken)
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	return runListen(transport, params)
}

// parseListenArgs parses the `mcp listen` argument list into the server name
// and the subscription filter params. At least one filter is required.
func parseListenArgs(args []string) (string, subscriptionsListenParams, bool, error) {
	var serverName string
	var tools, prompts, resources bool
	var resourceURIs []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tools":
			tools = true
		case "--prompts":
			prompts = true
		case "--resources":
			resources = true
		case "--resource":
			if i+1 >= len(args) {
				return "", subscriptionsListenParams{}, false, fmt.Errorf("--resource requires a value")
			}
			i++
			if err := validateResourceURI(args[i]); err != nil {
				return "", subscriptionsListenParams{}, false, err
			}
			resourceURIs = append(resourceURIs, args[i])
		case "--json":
			// Output is always JSON lines; accepted for consistency.
		case "--help", "-h":
			return "", subscriptionsListenParams{}, true, nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", subscriptionsListenParams{}, false, fmt.Errorf("unknown flag: %s", args[i])
			}
			if serverName == "" {
				serverName = args[i]
			} else {
				return "", subscriptionsListenParams{}, false, fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	if serverName == "" {
		return "", subscriptionsListenParams{}, false, fmt.Errorf("usage: mcp listen <server|url> [--tools] [--prompts] [--resources] [--resource <uri> ...]")
	}
	if !tools && !prompts && !resources && len(resourceURIs) == 0 {
		return "", subscriptionsListenParams{}, false, fmt.Errorf("at least one of --tools, --prompts, --resources, or --resource <uri> is required")
	}

	return serverName, subscriptionsListenParams{
		Notifications: subscriptionNotifications{
			ToolsListChanged:      tools,
			PromptsListChanged:    prompts,
			ResourcesListChanged:  resources,
			ResourceSubscriptions: resourceURIs,
		},
	}, false, nil
}

// runListen sends the subscriptions/listen request over a negotiated
// transport and prints one JSON line per stream event until the server
// closes the subscription.
func runListen(transport Transport, params subscriptionsListenParams) error {
	if _, ok := transport.(*protocolSession); !ok {
		// Legacy era: subscriptions/listen does not exist, and the removed
		// GET stream is not emulated.
		return listenUnsupportedError()
	}

	// The stream legitimately stays open indefinitely.
	transport.SetTimeout(0)

	reqID := nextID()
	cancelOnInterrupt(transport, reqID)

	resp, err := transport.SendStreaming(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      reqID,
		Method:  methodSubscriptionsListen,
		Params:  params,
	}, func(evt streamEvent) {
		printListenEvent(evt.Data, reqID)
	})
	if err != nil {
		// Stream EOF without the final response = abrupt disconnect.
		return fmt.Errorf("listen stream closed unexpectedly: %w", err)
	}
	if resp.Error != nil {
		if resp.Error.Code == codeMethodNotFound {
			return listenUnsupportedError()
		}
		return fmt.Errorf("subscriptions/listen: %s", resp.Error.Message)
	}

	// The final response (empty result) is the server's graceful close.
	data, _ := json.Marshal(listenOutput{Type: "closed"})
	_, _ = fmt.Fprintln(os.Stdout, string(data))
	return nil
}

func listenUnsupportedError() error {
	return fmt.Errorf("server does not support subscriptions/listen (pre-2026 protocol)")
}

// printListenEvent renders one stream event as a JSON line on stdout. Events
// that are not notifications, or that belong to a different subscription, are
// skipped with a warning.
func printListenEvent(data string, reqID int) {
	var notif struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(data), &notif); err != nil || notif.Method == "" {
		logStderr("warning: skipping non-notification stream event")
		return
	}

	var params struct {
		Meta          map[string]any  `json:"_meta"`
		Notifications json.RawMessage `json:"notifications"`
	}
	_ = json.Unmarshal(notif.Params, &params)
	if !subscriptionIDMatches(params.Meta[metaSubscriptionID], reqID) {
		logStderr("warning: skipping %s notification for a different subscription", notif.Method)
		return
	}

	out := listenOutput{Type: "notification", Method: notif.Method, Params: notif.Params}
	if notif.Method == methodSubscriptionAcknowledged {
		out = listenOutput{Type: "acknowledged", Notifications: params.Notifications}
	}
	line, err := json.Marshal(out)
	if err != nil {
		logStderr("warning: marshal listen event: %v", err)
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(line))
}

// subscriptionIDMatches reports whether a decoded _meta subscriptionId value
// refers to the listen request id. Servers echo the JSON-RPC id back either
// as the original number or as its string form.
func subscriptionIDMatches(v any, reqID int) bool {
	switch id := v.(type) {
	case float64:
		return id == float64(reqID)
	case string:
		return id == strconv.Itoa(reqID)
	}
	return false
}

// cancelOnInterrupt makes Ctrl-C cancel the listen subscription. Exiting the
// process closes an HTTP stream on its own (the spec's cancellation signal);
// a stdio child additionally gets a best-effort notifications/cancelled with
// the listen request id so it can stop producing events.
func cancelOnInterrupt(transport Transport, reqID int) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		if isStdioSession(transport) {
			_ = transport.Notify(jsonrpcNotification{
				JSONRPC: jsonrpcVersion,
				Method:  "notifications/cancelled",
				Params:  map[string]any{"requestId": reqID},
			})
		}
		os.Exit(130)
	}()
}

// isStdioSession reports whether a negotiated transport is a modern session
// over stdio (the only case where cancellation needs an explicit
// notification — closing the process already closes an HTTP stream).
func isStdioSession(transport Transport) bool {
	session, ok := transport.(*protocolSession)
	if !ok {
		return false
	}
	_, ok = session.transport.(*StdioTransport)
	return ok
}
