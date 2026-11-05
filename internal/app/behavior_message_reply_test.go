package app

import (
	"fmt"
	"net/http"
	"testing"
)

func TestBehaviorPrivateMessagesAcknowledgeSenderWithoutChangingRecipient(t *testing.T) {
	for _, recipient := range []string{"", "owner-openid"} {
		t.Run("recipient="+recipient, func(t *testing.T) {
			fixture := startBehaviorService(t, recipient, nil)
			allowed := map[string]bool{"Got it.": true, "Noted.": true, "Copy that.": true, "Received.": true}
			for index, content := range []string{"hello", fixture.apiKey, "你好 👋", ""} {
				messageID := fmt.Sprintf("private-message-%d", index)
				if err := fixture.deliverMessageWithID(t, "visitor", content, messageID); err != nil {
					t.Fatal(err)
				}
				message := fixture.messages[index]
				text, _ := message["content"].(string)
				if !allowed[text] || message["recipient"] != "visitor" || message["msg_id"] != messageID || message["msg_seq"] != float64(1) || message["msg_type"] != float64(0) || message["markdown"] != nil || message["keyboard"] != nil {
					t.Fatal("private message did not receive a plain acknowledgement addressed to its sender")
				}
			}
			status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
			requireStatus(t, status, http.StatusOK)
			if result["user_openid"] != recipient || fixture.sendCalls.Load() != 4 {
				t.Fatal("ordinary messages changed pairing or sent more than one reply each")
			}
		})
	}
}

func TestBehaviorBindingCommandsExplainRejectedInput(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	for index, scenario := range []struct{ content, reply string }{
		{"bind", "Usage: bind <pairing_code>"},
		{"bind first second", "Usage: bind <pairing_code>"},
		{"bind invalid-code", "Pairing code is invalid or expired. Please generate a new one."},
		{"bind " + fixture.apiKey, "Pairing code is invalid or expired. Please generate a new one."},
	} {
		messageID := fmt.Sprintf("binding-message-%d", index)
		if err := fixture.deliverMessageWithID(t, "visitor", scenario.content, messageID); err != nil {
			t.Fatal(err)
		}
		message := fixture.messages[index]
		if message["content"] != scenario.reply || message["msg_id"] != messageID || message["recipient"] != "visitor" || message["markdown"] != nil {
			t.Fatal("invalid binding command did not receive the appropriate private reply")
		}
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != false {
		t.Fatal("invalid command changed the recipient")
	}
}

func TestBehaviorPrivateMessagesRequireReplyMetadata(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	for _, scenario := range []struct{ user, messageID string }{{"visitor", ""}, {"", "private-message"}} {
		if err := fixture.deliverMessageWithID(t, scenario.user, "hello", scenario.messageID); err == nil {
			t.Fatal("message with no sender or message ID was accepted")
		}
	}
	if fixture.sendCalls.Load() != 0 {
		t.Fatal("invalid message metadata caused an outbound reply")
	}
}
