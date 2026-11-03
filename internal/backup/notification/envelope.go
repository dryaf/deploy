package notification

import (
	"unicode/utf8"
)

func NotificationEnvelope(message string) string {
	if len(message) <= 3000 {
		return message
	}
	message = message[:2850]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message + "\nAdditional detail omitted; inspect backup-worker status/logs."
}

// Re-serialize after clipping instead of slicing an encoded JSON string.
func boundedNotificationEvent(event NotificationEvent) NotificationEvent {
	event.Message = NotificationEnvelope(event.Message)
	return event
}
