package app

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/samber/oops"

	"github.com/ceclin/notify-qq/internal/logging"
	"github.com/ceclin/notify-qq/internal/qq"
)

const maxContentBytes = 4000
const maxButtons = 5
const maxButtonLabelRunes = 10
const maxButtonURLBytes = 2048

type notification struct {
	Content string               `json:"content"`
	Buttons []notificationButton `json:"buttons"`
}

type notificationButton struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func (input notification) compile(ctx context.Context) (qq.Notification, error) {
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return qq.Notification{}, invalidNotification(ctx, "content_required", "content must not be empty")
	}
	if !utf8.ValidString(content) || len(content) > maxContentBytes {
		return qq.Notification{}, invalidNotification(ctx, "content_too_long", "Markdown content must fit within 4000 UTF-8 bytes")
	}
	if len(input.Buttons) > maxButtons {
		return qq.Notification{}, invalidNotification(ctx, "buttons_invalid", "At most 5 link buttons are allowed")
	}
	message := qq.Notification{Markdown: content}
	for index, inputButton := range input.Buttons {
		button := qq.LinkButton{Label: strings.TrimSpace(inputButton.Label), URL: strings.TrimSpace(inputButton.URL)}
		attributes := []slog.Attr{slog.Int("button_index", index)}
		if button.Label == "" || !utf8.ValidString(button.Label) || utf8.RuneCountInString(button.Label) > maxButtonLabelRunes || strings.ContainsFunc(button.Label, unicode.IsControl) {
			return qq.Notification{}, invalidNotification(ctx, "button_label_invalid", "Button labels must contain 1 to 10 characters without control characters", attributes...)
		}
		parsed, err := url.Parse(button.URL)
		// Parser errors can contain the supplied URL, including private query data.
		// Return a fixed diagnostic without attaching those errors to logs.
		if err != nil || len(button.URL) > maxButtonURLBytes || strings.ContainsFunc(button.URL, func(character rune) bool { return unicode.IsSpace(character) || unicode.IsControl(character) }) || parsed == nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.Opaque != "" {
			return qq.Notification{}, invalidNotification(ctx, "button_url_invalid", "Button URLs must be absolute HTTP(S) URLs without credentials or whitespace, at most 2048 UTF-8 bytes", attributes...)
		}
		message.Buttons = append(message.Buttons, button)
	}
	return message, nil
}

func invalidNotification(ctx context.Context, code, public string, attributes ...slog.Attr) error {
	return logging.WithErrorAttributes(oops.FromContext(ctx).In("notification").Code(code).Public(public), attributes...).New("Notification input is invalid")
}
