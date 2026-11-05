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
			contents := []string{"hello", fixture.apiKey, "你好 👋", "", "bind " + fixture.pairingCode, "code: " + fixture.pairingCode}
			for index, content := range contents {
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
			if result["user_openid"] != recipient || int(fixture.sendCalls.Load()) != len(contents) {
				t.Fatal("ordinary messages changed pairing or sent more than one reply each")
			}
		})
	}
}

func TestBehaviorInvalidPairingCodesReceiveFailureReply(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	for index, scenario := range []struct{ content, reply string }{
		{"nqp_", "Pairing code is invalid or expired. Please generate a new one."},
		{"nqp_invalid-code", "Pairing code is invalid or expired. Please generate a new one."},
		{fixture.pairingCode + " extra", "Pairing code is invalid or expired. Please generate a new one."},
	} {
		messageID := fmt.Sprintf("binding-message-%d", index)
		if err := fixture.deliverMessageWithID(t, "visitor", scenario.content, messageID); err != nil {
			t.Fatal(err)
		}
		message := fixture.messages[index]
		if message["content"] != scenario.reply || message["msg_id"] != messageID || message["recipient"] != "visitor" || message["markdown"] != nil {
			t.Fatal("invalid pairing code did not receive the appropriate private reply")
		}
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != false {
		t.Fatal("invalid pairing code changed the recipient")
	}
}

func TestBehaviorPastedPairingCodeTrimsSurroundingWhitespace(t *testing.T) {
	fixture := startBehaviorService(t, "", nil)
	if err := fixture.deliverMessage(t, "owner-openid", " \t\n"+fixture.pairingCode+"\n\t "); err != nil {
		t.Fatal(err)
	}
	status, result, _ := fixture.request(t, http.MethodGet, "/status", "", map[string]string{"Authorization": "Bearer " + fixture.apiKey})
	requireStatus(t, status, http.StatusOK)
	if result["paired"] != true || result["user_openid"] != "owner-openid" || fixture.sendCalls.Load() != 1 {
		t.Fatal("pasted pairing code did not bind its sender")
	}
	if fixture.messages[0]["content"] != "Paired successfully. Notifications are ready." || fixture.messages[0]["msg_id"] != "binding-message" {
		t.Fatal("pasted pairing code did not receive its confirmation")
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
