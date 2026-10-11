package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ceclin/notify-qq/internal/logging"
)

func TestBehaviorMarkdownAndLinkButtonsReachQQ(t *testing.T) {
	fixture := startBehaviorServiceWithPrefix(t, "owner-openid", nil, "/api")
	input := map[string]any{
		"content": "  ## Backup [daily] *report*\n\n**Success**\n\n- Database uploaded\n\n![Chart #200px #100px](https://example.com/chart.png)  ",
		"buttons": []map[string]string{
			{"label": " 日志 ", "url": " https://example.com/logs?job=backup#latest "},
			{"label": "Details", "url": "http://example.com/details"},
			{"label": "Status", "url": "https://example.com/status"},
			{"label": "Metrics", "url": "https://example.com/metrics"},
			{"label": "Help", "url": "https://example.com/help"},
		},
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	status, _, headers := fixture.notify(t, string(payload))
	requireStatus(t, status, http.StatusOK)
	if headers.Get("X-Request-ID") == "" {
		t.Fatal("Markdown response lost correlation")
	}
	if fixture.sendCalls.Load() != 1 {
		t.Fatal("Markdown notification was sent more than once")
	}
	message := fixture.messages[0]
	if message["msg_type"] != float64(2) || message["recipient"] != "owner-openid" {
		t.Fatal("Markdown notification used wrong message type or recipient")
	}
	for _, field := range []string{"content", "ark", "msg_id", "event_id"} {
		if _, present := message[field]; present {
			t.Fatalf("unexpected QQ payload field %s", field)
		}
	}
	markdown, ok := message["markdown"].(map[string]any)
	if !ok {
		t.Fatal("QQ request had no Markdown envelope")
	}
	expected := "## Backup [daily] *report*\n\n**Success**\n\n- Database uploaded\n\n![Chart #200px #100px](https://example.com/chart.png)"
	if markdown["content"] != expected {
		t.Fatalf("wrong Markdown content: %v", markdown)
	}
	rows := message["keyboard"].(map[string]any)["content"].(map[string]any)["rows"].([]any)
	if len(rows) != 3 {
		t.Fatal("five links were not arranged in three rows")
	}
	index := 0
	expectedLabels := []string{"日志", "Details", "Status", "Metrics", "Help"}
	expectedURLs := []string{"https://example.com/logs?job=backup#latest", "http://example.com/details", "https://example.com/status", "https://example.com/metrics", "https://example.com/help"}
	for rowIndex, row := range rows {
		buttons := row.(map[string]any)["buttons"].([]any)
		size := 2
		if rowIndex == 2 {
			size = 1
		}
		if len(buttons) != size {
			t.Fatal("keyboard row has incorrect button count")
		}
		for _, raw := range buttons {
			button := raw.(map[string]any)
			render := button["render_data"].(map[string]any)
			action := button["action"].(map[string]any)
			if button["id"] != "notify_"+strconv.Itoa(index+1) || render["label"] != expectedLabels[index] || render["style"] != float64(1) {
				t.Fatal("link label, ID, or style not mapped correctly")
			}
			if action["type"] != float64(0) || action["data"] != expectedURLs[index] || action["permission"].(map[string]any)["type"] != float64(2) {
				t.Fatal("link target or action not mapped correctly")
			}
			index++
		}
	}
}

func TestBehaviorMarkdownWithoutButtonsOmitsKeyboard(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	for _, body := range []string{`{"content":"# Report\n\n**Success**"}`, `{"content":"# Report\n\n**Success**","buttons":null}`, `{"content":"# Report\n\n**Success**","buttons":[]}`} {
		status, _, _ := fixture.notify(t, body)
		requireStatus(t, status, http.StatusOK)
		message := fixture.messages[len(fixture.messages)-1]
		if message["msg_type"] != float64(2) || message["markdown"].(map[string]any)["content"] != "# Report\n\n**Success**" {
			t.Fatal("content-only notification did not remain Markdown")
		}
		if _, present := message["keyboard"]; present {
			t.Fatal("empty buttons produced a keyboard")
		}
		if _, present := message["content"]; present {
			t.Fatal("Markdown notification used the QQ plain-text field")
		}
	}
}

func TestBehaviorInvalidMarkdownButtonsNeverContactQQ(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	cases := []struct {
		name  string
		input map[string]any
		code  string
	}{
		{"removed title", map[string]any{"title": "Backup", "content": "report"}, "invalid_json"},
		{"removed null title", map[string]any{"title": nil, "content": "report"}, "invalid_json"},
		{"too many buttons", map[string]any{"content": "report", "buttons": []any{1, 2, 3, 4, 5, 6}}, "invalid_json"},
		{"empty label", buttonInput("  ", "https://example.com"), "button_label_invalid"},
		{"long unicode label", buttonInput(strings.Repeat("报", 11), "https://example.com"), "button_label_invalid"},
		{"label control character", buttonInput("line\nbreak", "https://example.com"), "button_label_invalid"},
		{"empty URL", buttonInput("Logs", ""), "button_url_invalid"},
		{"relative URL", buttonInput("Logs", "/logs"), "button_url_invalid"},
		{"script URL", buttonInput("Logs", "javascript:alert(1)"), "button_url_invalid"},
		{"other scheme", buttonInput("Logs", "ftp://example.com"), "button_url_invalid"},
		{"credentials", buttonInput("Logs", "https://user:private-password@example.com"), "button_url_invalid"},
		{"host missing", buttonInput("Logs", "https:///path"), "button_url_invalid"},
		{"bad escape", buttonInput("Logs", "https://example.com/%private-token"), "button_url_invalid"},
		{"embedded whitespace", buttonInput("Logs", "https://example.com/private path"), "button_url_invalid"},
		{"long URL", buttonInput("Logs", "https://example.com/"+strings.Repeat("x", 2048)), "button_url_invalid"},
		{"callback override", map[string]any{"content": "report", "buttons": []any{map[string]any{"label": "Logs", "url": "https://example.com", "action": map[string]any{"type": 1}}}}, "invalid_json"},
		{"raw keyboard", map[string]any{"content": "report", "keyboard": map[string]any{}}, "invalid_json"},
		{"plain-text selector", map[string]any{"content": "report", "msg_type": 0}, "invalid_json"},
	}
	buttons := make([]map[string]string, 6)
	for index := range buttons {
		buttons[index] = map[string]string{"label": "Logs", "url": "https://example.com"}
	}
	cases = append(cases, struct {
		name  string
		input map[string]any
		code  string
	}{"six valid buttons", map[string]any{"content": "report", "buttons": buttons}, "buttons_invalid"})
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) {
			body, err := json.Marshal(scenario.input)
			if err != nil {
				t.Fatal(err)
			}
			status, result, _ := fixture.notify(t, string(body))
			requireStatus(t, status, http.StatusBadRequest)
			if result["error"].(map[string]any)["code"] != scenario.code {
				t.Fatalf("error code=%v, want %s", result, scenario.code)
			}
		})
	}
	if fixture.sendCalls.Load() != 0 || fixture.tokenCalls.Load() != 0 {
		t.Fatal("invalid Markdown/buttons contacted QQ")
	}
}

func TestBehaviorMarkdownByteLimit(t *testing.T) {
	fixture := startBehaviorService(t, "owner-openid", nil)
	for _, size := range []int{4000, 4001} {
		body, err := json.Marshal(map[string]string{"content": strings.Repeat("x", size)})
		if err != nil {
			t.Fatal(err)
		}
		status, _, _ := fixture.notify(t, string(body))
		expected := http.StatusOK
		if size > 4000 {
			expected = http.StatusBadRequest
		}
		requireStatus(t, status, expected)
	}
	status, _, _ := fixture.notify(t, `{"content":"`+strings.Repeat("报", 1334)+`"}`)
	requireStatus(t, status, http.StatusBadRequest)
	if fixture.sendCalls.Load() != 1 {
		t.Fatal("over-limit Markdown reached QQ")
	}
}

func TestBehaviorMarkdownAndButtonLogsOmitPrivateData(t *testing.T) {
	var captured bytes.Buffer
	logger := logging.NewLogger(&captured, slog.LevelDebug)
	fixture := startBehaviorServiceWithLogger(t, "owner-openid", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = w.Write([]byte(`{"access_token":"test-token","expires_in":7200}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":40034124,"message":"QQ rejected private-markdown-body and private-button-label"}`))
	}, logger)
	status, _, _ := fixture.notify(t, `{"content":"## private-heading\n\n**private-markdown-body**","buttons":[{"label":"私密日志","url":"https://example.com/?token=private-link-token"}]}`)
	requireStatus(t, status, http.StatusBadGateway)
	invalid, err := json.Marshal(buttonInput("私密日志", "https://example.com/%private-parser-token"))
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ = fixture.notify(t, string(invalid))
	requireStatus(t, status, http.StatusBadRequest)
	for _, secret := range []string{"private-heading", "private-markdown-body", "private-button-label", "私密日志", "private-link-token", "private-parser-token"} {
		if strings.Contains(captured.String(), secret) {
			t.Fatal("Markdown/button information leaked in logs")
		}
	}
	decoder := json.NewDecoder(strings.NewReader(captured.String()))
	started, errors := 0, 0
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "notification_started" {
			started++
			if event["format"] != "markdown" || event["button_count"] != float64(1) || event["api_key_id"] == nil {
				t.Fatal("Markdown workflow lost format/button/key fields")
			}
		}
		if event["level"] == "ERROR" {
			errors++
		}
	}
	if started != 1 || errors != 1 || fixture.sendCalls.Load() != 1 {
		t.Fatal("Markdown rejection was replayed or logged multiple times")
	}
}

func buttonInput(label, target string) map[string]any {
	return map[string]any{"content": "report", "buttons": []map[string]string{{"label": label, "url": target}}}
}
