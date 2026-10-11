package qq

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"
	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/logging"
)

const c2cMessageIntent = 1 << 25
const gatewayOperationTimeout = 10 * time.Second

type Event struct {
	Type     string
	ID       string
	Sequence int64
	Data     json.RawMessage
}

type gatewayFrame struct {
	Opcode   *int            `json:"op"`
	Sequence *int64          `json:"s"`
	Type     string          `json:"t"`
	ID       string          `json:"id"`
	Data     json.RawMessage `json:"d"`
}

type gatewaySession struct {
	id       string
	sequence atomic.Int64
}

type gatewayFailure struct {
	error
	resetSession bool
	terminal     bool
	retryAfter   time.Duration
}

func (e *gatewayFailure) Unwrap() error { return e.error }

func (q *Client) GatewayConnected() bool { return q.gatewayConnected.Load() }

// The caller owns cancellation and must wait for Listen to return. Its worker
// boundary owns retry/error reporting; event handlers only return enriched errors.
func (q *Client) Listen(ctx context.Context, handleEvent func(context.Context, Event) error) error {
	var result error
	panicError := oops.In("qq_gateway").Code("gateway_panic").Recover(func() {
		result = q.listen(ctx, handleEvent)
	})
	if panicError != nil {
		result = panicError
	}
	q.gatewayConnected.Store(false)
	if result != nil {
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelError, "QQ event worker failed",
			slog.String("event", "qq_gateway_failed"), logging.ErrorAttribute(result))
	} else {
		logging.FromContext(ctx).LogAttrs(ctx, slog.LevelInfo, "QQ event worker stopped", slog.String("event", "qq_gateway_stopped"))
	}
	return result
}

func (q *Client) listen(ctx context.Context, handleEvent func(context.Context, Event) error) error {
	session := &gatewaySession{}
	session.sequence.Store(-1)
	failures := 0
	for ctx.Err() == nil {
		connectionID := ulid.Make().String()
		logger := slog.New(logging.FromContext(ctx).Handler().WithAttrs([]slog.Attr{slog.String("connection_id", connectionID)}))
		connectionCtx := logging.WithLogger(ctx, logger)
		connectionCtx = oops.WithBuilder(connectionCtx, logging.WithErrorAttributes(oops.In("qq_gateway").Trace(connectionID), slog.String("connection_id", connectionID)))
		started := time.Now()
		err := q.connectGateway(connectionCtx, session, handleEvent)
		if ctx.Err() != nil {
			return nil
		}
		var policy *gatewayFailure
		if errors.As(err, &policy) {
			if policy.terminal {
				return err
			}
			if policy.resetSession {
				session.id = ""
				session.sequence.Store(-1)
			}
		}
		if time.Since(started) > time.Minute {
			failures = 0
		}
		failures++
		delay := min(30*time.Second, time.Duration(float64(time.Second<<min(failures-1, 5))*(0.8+0.4*rand.Float64())))
		if policy != nil && policy.retryAfter > delay {
			delay = policy.retryAfter
		}
		var upstream *Error
		if errors.As(err, &upstream) && upstream.RateLimited() {
			delay = max(delay, gatewayRateLimitDelay(upstream.RetryAfter))
		}
		level := slog.LevelDebug
		// Powers-of-two reporting preserves the retry history without repeating
		// the same outage every few seconds. Each recovery remains an INFO event.
		if failures&(failures-1) == 0 {
			level = slog.LevelWarn
		}
		logger.LogAttrs(ctx, level, "QQ gateway reconnect scheduled",
			slog.String("event", "qq_gateway_retry"), slog.Int("attempt", failures),
			slog.Bool("resume", session.id != ""), slog.Float64("retry_delay_ms", logging.Milliseconds(delay)),
			logging.ErrorAttribute(err))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func gatewayRateLimitDelay(retryAfter string) time.Duration {
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds >= 0 && seconds <= 86400 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(retryAfter); err == nil {
		return max(0, min(24*time.Hour, time.Until(deadline)))
	}
	return time.Minute
}

func (q *Client) discoverGateway(ctx context.Context, session *gatewaySession) (string, string, error) {
	token, err := q.accessToken(ctx)
	if err != nil {
		return "", "", err
	}
	data, err := q.request(ctx, http.MethodGet, "gateway", apiBaseURL+"/gateway/bot", token, nil)
	if err != nil {
		var upstream *Error
		if errors.As(err, &upstream) && upstream.Status == http.StatusUnauthorized {
			q.invalidateToken(ctx, token)
		}
		return "", "", err
	}
	var gateway struct {
		URL   string `json:"url"`
		Limit *struct {
			Remaining  int   `json:"remaining"`
			ResetAfter int64 `json:"reset_after"`
		} `json:"session_start_limit"`
	}
	if json.Unmarshal(data, &gateway) != nil {
		return "", "", gatewayError(ctx, "gateway_invalid_response", "QQ gateway discovery response is invalid")
	}
	endpoint, err := url.Parse(gateway.URL)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return "", "", gatewayError(ctx, "gateway_invalid_url", "QQ gateway must provide an absolute WSS URL without credentials or fragment")
	}
	if session.id == "" && gateway.Limit != nil && gateway.Limit.Remaining <= 0 {
		delay := time.Minute
		if gateway.Limit.ResetAfter > 0 && gateway.Limit.ResetAfter <= int64(24*time.Hour/time.Millisecond) {
			delay = time.Duration(gateway.Limit.ResetAfter) * time.Millisecond
		}
		return "", "", &gatewayFailure{error: gatewayError(ctx, "gateway_session_limit", "QQ gateway session start limit reached"), retryAfter: delay}
	}
	return endpoint.String(), token, nil
}

func (q *Client) connectGateway(ctx context.Context, session *gatewaySession, handleEvent func(context.Context, Event) error) error {
	endpoint, token, err := q.discoverGateway(ctx, session)
	if err != nil {
		return err
	}
	logger := logging.FromContext(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "Connecting to QQ gateway", slog.String("event", "qq_gateway_connecting"))
	httpClient := *q.http
	// HTTP timeouts bound the upgrade only; a WebSocket lives until cancellation.
	httpClient.Timeout = 0
	dialCtx, cancelDial := context.WithTimeout(ctx, gatewayOperationTimeout)
	connection, _, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPClient: &httpClient})
	cancelDial()
	if err != nil {
		return gatewayError(ctx, "gateway_dial_failed", "QQ gateway connection could not be established")
	}
	connection.SetReadLimit(1 << 20)
	connectionCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		q.gatewayConnected.Store(false)
		cancel()
		_ = connection.CloseNow()
		workers.Wait()
	}()
	helloCtx, cancelHello := context.WithTimeout(connectionCtx, gatewayOperationTimeout)
	hello, err := readGatewayFrame(helloCtx, connection)
	cancelHello()
	if err != nil {
		return err
	}
	var heartbeat struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if *hello.Opcode != 10 || json.Unmarshal(hello.Data, &heartbeat) != nil || heartbeat.Interval <= 0 || heartbeat.Interval > 300000 {
		return gatewayError(ctx, "gateway_invalid_hello", "QQ gateway Hello has no valid heartbeat interval")
	}
	if session.id == "" {
		err = writeGatewayFrame(connectionCtx, connection, 2, struct {
			Token   string `json:"token"`
			Intents int    `json:"intents"`
			Shard   [2]int `json:"shard"`
		}{"QQBot " + token, c2cMessageIntent, [2]int{0, 1}})
	} else {
		err = writeGatewayFrame(connectionCtx, connection, 6, struct {
			Token     string `json:"token"`
			SessionID string `json:"session_id"`
			Sequence  int64  `json:"seq"`
		}{"QQBot " + token, session.id, session.sequence.Load()})
	}
	if err != nil {
		return err
	}
	logger.LogAttrs(ctx, slog.LevelDebug, "QQ gateway authentication sent", slog.String("event", "qq_gateway_authenticating"), slog.Bool("resume", session.id != ""))
	frames := make(chan gatewayFrame)
	workerErrors := make(chan error, 2)
	events := make(chan Event, 16)
	workers.Go(func() {
		panicError := oops.FromContext(connectionCtx).Code("gateway_reader_panic").Recover(func() {
			for {
				frame, err := readGatewayFrame(connectionCtx, connection)
				if err != nil {
					workerErrors <- err
					return
				}
				select {
				case frames <- frame:
				case <-connectionCtx.Done():
					return
				}
			}
		})
		if panicError != nil {
			workerErrors <- &gatewayFailure{error: panicError, terminal: true}
		}
	})
	workers.Go(func() {
		panicError := oops.FromContext(connectionCtx).Code("gateway_handler_panic").Recover(func() {
			if err := processGatewayEvents(connectionCtx, session, events, handleEvent); err != nil {
				workerErrors <- err
			}
		})
		if panicError != nil {
			workerErrors <- &gatewayFailure{error: panicError, terminal: true}
		}
	})
	ticker := time.NewTicker(time.Duration(heartbeat.Interval) * time.Millisecond)
	defer ticker.Stop()
	authDeadline := time.NewTimer(gatewayOperationTimeout)
	defer authDeadline.Stop()
	authenticated, awaitingAck := false, false
	for {
		select {
		case <-connectionCtx.Done():
			return connectionCtx.Err()
		case err := <-workerErrors:
			return err
		case <-authDeadline.C:
			if !authenticated {
				return gatewayError(ctx, "gateway_auth_timeout", "QQ gateway authentication timed out")
			}
		case <-ticker.C:
			if awaitingAck {
				return gatewayError(ctx, "gateway_heartbeat_timeout", "QQ gateway did not acknowledge the heartbeat")
			}
			if err := sendGatewayHeartbeat(connectionCtx, connection, session); err != nil {
				return err
			}
			awaitingAck = true
		case frame := <-frames:
			switch *frame.Opcode {
			case 0:
				if frame.Sequence == nil || *frame.Sequence < 0 {
					return gatewayError(ctx, "gateway_invalid_event", "QQ gateway event has no valid sequence")
				}
				if frame.Type == "READY" {
					var ready struct {
						SessionID string `json:"session_id"`
					}
					if authenticated || session.id != "" || json.Unmarshal(frame.Data, &ready) != nil || ready.SessionID == "" {
						return gatewayError(ctx, "gateway_invalid_ready", "QQ gateway Ready event is invalid")
					}
					session.id = ready.SessionID
					// READY cannot have earlier queued events in a new session;
					// retain its cursor even if the socket closes immediately.
					session.sequence.Store(*frame.Sequence)
				} else if frame.Type == "RESUMED" && (authenticated || session.id == "") {
					return gatewayError(ctx, "gateway_invalid_resume", "QQ gateway Resumed event is unexpected")
				}
				if frame.Type == "READY" || frame.Type == "RESUMED" {
					authenticated = true
					authDeadline.Stop()
					q.gatewayConnected.Store(true)
					logger.LogAttrs(ctx, slog.LevelInfo, "QQ gateway connected", slog.String("event", "qq_gateway_connected"), slog.Bool("resumed", frame.Type == "RESUMED"))
				} else if session.id == "" {
					return gatewayError(ctx, "gateway_event_before_ready", "QQ gateway sent an event before establishing a session")
				}
				select {
				case events <- Event{Type: frame.Type, ID: frame.ID, Sequence: *frame.Sequence, Data: frame.Data}:
				default:
					return gatewayError(ctx, "gateway_queue_full", "QQ gateway event queue is full")
				}
			case 1:
				if err := sendGatewayHeartbeat(connectionCtx, connection, session); err != nil {
					return err
				}
				awaitingAck = true
			case 7:
				return gatewayError(ctx, "gateway_reconnect_requested", "QQ gateway requested reconnection")
			case 9:
				q.invalidateToken(ctx, token)
				return &gatewayFailure{error: gatewayError(ctx, "gateway_invalid_session", "QQ gateway rejected the session"), resetSession: true}
			case 11:
				awaitingAck = false
				logger.LogAttrs(ctx, slog.LevelDebug, "QQ gateway heartbeat acknowledged", slog.String("event", "qq_gateway_heartbeat_ack"))
			default:
				return gatewayError(ctx, "gateway_invalid_opcode", "QQ gateway sent an unsupported opcode")
			}
		}
	}
}

// Advance the resume cursor after successful processing, including READY and
// RESUMED markers. Otherwise a fast RESUMED could skip still-queued events.
func processGatewayEvents(ctx context.Context, session *gatewaySession, events <-chan Event, handleEvent func(context.Context, Event) error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			if ctx.Err() != nil {
				return nil
			}
			if event.Sequence <= session.sequence.Load() {
				continue
			}
			if event.Type != "READY" && event.Type != "RESUMED" && handleEvent != nil {
				logger := slog.New(logging.FromContext(ctx).Handler().WithAttrs([]slog.Attr{
					slog.String("qq_event_id", event.ID), slog.String("qq_event_type", event.Type), slog.Int64("qq_sequence", event.Sequence),
				}))
				eventCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				eventCtx = logging.WithLogger(eventCtx, logger)
				eventCtx = oops.WithBuilder(eventCtx, logging.WithErrorAttributes(oops.FromContext(ctx), slog.String("qq_event_id", event.ID), slog.String("qq_event_type", event.Type), slog.Int64("qq_sequence", event.Sequence)))
				logger.LogAttrs(eventCtx, slog.LevelDebug, "QQ event processing started", slog.String("event", "qq_event_started"))
				started := time.Now()
				err := handleEvent(eventCtx, event)
				cancel()
				if err != nil {
					return oops.FromContext(eventCtx).In("qq_gateway").Code("gateway_event_failed").Since(started).Wrapf(err, "QQ event processing failed")
				}
				logger.LogAttrs(ctx, slog.LevelDebug, "QQ event processing completed", slog.String("event", "qq_event_completed"), slog.Float64("duration_ms", logging.Milliseconds(time.Since(started))))
			}
			session.sequence.Store(event.Sequence)
		}
	}
}

func sendGatewayHeartbeat(ctx context.Context, connection *websocket.Conn, session *gatewaySession) error {
	var sequence *int64
	if value := session.sequence.Load(); value >= 0 {
		sequence = &value
	}
	if err := writeGatewayFrame(ctx, connection, 1, sequence); err != nil {
		return err
	}
	logging.FromContext(ctx).LogAttrs(ctx, slog.LevelDebug, "QQ gateway heartbeat sent", slog.String("event", "qq_gateway_heartbeat_sent"), slog.Any("qq_sequence", sequence))
	return nil
}

func writeGatewayFrame(ctx context.Context, connection *websocket.Conn, opcode int, data any) error {
	payload, err := json.Marshal(struct {
		Opcode int `json:"op"`
		Data   any `json:"d"`
	}{opcode, data})
	if err != nil {
		return gatewayError(ctx, "gateway_encode_failed", "QQ gateway payload could not be encoded")
	}
	writeCtx, cancel := context.WithTimeout(ctx, gatewayOperationTimeout)
	defer cancel()
	if err := connection.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return gatewayWireError(ctx, err)
	}
	return nil
}

func readGatewayFrame(ctx context.Context, connection *websocket.Conn) (gatewayFrame, error) {
	var frame gatewayFrame
	kind, data, err := connection.Read(ctx)
	if err != nil {
		return frame, gatewayWireError(ctx, err)
	}
	if kind != websocket.MessageText || json.Unmarshal(data, &frame) != nil || frame.Opcode == nil {
		return frame, gatewayError(ctx, "gateway_invalid_frame", "QQ gateway returned an invalid JSON text frame")
	}
	return frame, nil
}

func gatewayError(ctx context.Context, code, message string) error {
	return logging.OperationError(ctx, "qq_gateway", code, message, nil)
}

func gatewayWireError(ctx context.Context, cause error) error {
	if ctx.Err() != nil {
		return logging.OperationError(ctx, "qq_gateway", "gateway_io_canceled", "QQ gateway I/O was canceled or timed out", ctx.Err())
	}
	code := int(websocket.CloseStatus(cause))
	// Close reasons and library errors can contain server-controlled text or
	// authentication URLs. Preserve the close code without logging those values.
	err := logging.WithErrorAttributes(oops.FromContext(ctx).In("qq_gateway").Code("gateway_disconnected"), slog.Int("close_code", code)).New("QQ gateway connection closed")
	policy := &gatewayFailure{error: err}
	switch code {
	case 4001, 4002, 4010, 4011, 4012, 4013, 4014, 4914, 4915:
		policy.terminal = true
	case 4006, 4007:
		policy.resetSession = true
	default:
		policy.resetSession = code >= 4000 && code != 4008 && code != 4009
	}
	return policy
}
