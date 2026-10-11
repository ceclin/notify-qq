package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
)

// Each delivery crosses a real local WSS connection and the production event
// worker. A bounded lifetime keeps storage-failure scenarios independent.
func (fixture *behaviorFixture) deliverMessage(t *testing.T, user, content string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = logging.WithLogger(ctx, fixture.logger)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = connection.CloseNow() }()
		if err := wsjson.Write(ctx, connection, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 100}}); err != nil {
			return
		}
		var identify struct {
			Opcode int `json:"op"`
			Data   struct {
				Token   string `json:"token"`
				Intents int    `json:"intents"`
				Shard   [2]int `json:"shard"`
			} `json:"d"`
		}
		if err := wsjson.Read(ctx, connection, &identify); err != nil {
			return
		}
		if identify.Opcode != 2 || identify.Data.Token != "QQBot test-token" || identify.Data.Intents != 1<<25 || identify.Data.Shard != [2]int{0, 1} {
			t.Error("gateway did not identify with the C2C intent and configured token")
			return
		}
		if err := wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]string{"session_id": "test-session"}}); err != nil {
			return
		}
		if err := wsjson.Write(ctx, connection, map[string]any{"op": 0, "s": 2, "t": "C2C_MESSAGE_CREATE", "id": "binding-event", "d": map[string]any{"content": content, "author": map[string]string{"user_openid": user}}}); err != nil {
			return
		}
		for {
			var frame map[string]json.RawMessage
			if wsjson.Read(ctx, connection, &frame) != nil {
				return
			}
			if wsjson.Write(ctx, connection, map[string]int{"op": 11}) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	fixture.gatewayURL = "wss" + strings.TrimPrefix(upstream.URL, "https")
	result := make(chan error, 1)
	done := make(chan error, 1)
	service := fixture.service
	go func() {
		done <- service.qq.Listen(ctx, func(ctx context.Context, event qq.Event) error {
			err := service.handleQQEvent(ctx, event)
			result <- err
			return err
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal("QQ gateway did not deliver the pairing event")
		return ctx.Err()
	}
}
