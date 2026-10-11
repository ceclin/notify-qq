package qq

import "strconv"

type Notification struct {
	Markdown string
	Buttons  []LinkButton
}

type LinkButton struct {
	Label string
	URL   string
}

const (
	messageTypeMarkdown      = 2
	buttonActionLink         = 0
	buttonPermissionEveryone = 2
	buttonStyleBlue          = 1
	buttonsPerRow            = 2
)

type sendMessageRequest struct {
	MsgType  int             `json:"msg_type"`
	Markdown markdownContent `json:"markdown"`
	Keyboard *keyboard       `json:"keyboard,omitempty"`
}

type markdownContent struct {
	Content string `json:"content"`
}
type keyboard struct {
	Content keyboardContent `json:"content"`
}
type keyboardContent struct {
	Rows []buttonRow `json:"rows"`
}
type buttonRow struct {
	Buttons []linkButton `json:"buttons"`
}
type linkButton struct {
	ID         string           `json:"id"`
	RenderData buttonRenderData `json:"render_data"`
	Action     buttonAction     `json:"action"`
}
type buttonRenderData struct {
	Label string `json:"label"`
	Style int    `json:"style"`
}
type buttonAction struct {
	Type       int              `json:"type"`
	Permission buttonPermission `json:"permission"`
	Data       string           `json:"data"`
}
type buttonPermission struct {
	Type int `json:"type"`
}

// QQ's Markdown and keyboard envelopes stay in the protocol package so callers
// only supply content and links, without controlling permissions or actions.
func messagePayload(notification Notification) sendMessageRequest {
	request := sendMessageRequest{MsgType: messageTypeMarkdown, Markdown: markdownContent{Content: notification.Markdown}}
	if len(notification.Buttons) == 0 {
		return request
	}
	rows := make([]buttonRow, 0, (len(notification.Buttons)+buttonsPerRow-1)/buttonsPerRow)
	for start := 0; start < len(notification.Buttons); start += buttonsPerRow {
		buttons := make([]linkButton, 0, buttonsPerRow)
		for index := start; index < min(start+buttonsPerRow, len(notification.Buttons)); index++ {
			button := notification.Buttons[index]
			buttons = append(buttons, linkButton{
				ID:         "notify_" + strconv.Itoa(index+1),
				RenderData: buttonRenderData{Label: button.Label, Style: buttonStyleBlue},
				Action:     buttonAction{Type: buttonActionLink, Permission: buttonPermission{Type: buttonPermissionEveryone}, Data: button.URL},
			})
		}
		rows = append(rows, buttonRow{Buttons: buttons})
	}
	request.Keyboard = &keyboard{Content: keyboardContent{Rows: rows}}
	return request
}
