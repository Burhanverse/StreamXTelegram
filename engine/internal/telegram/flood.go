package telegram

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"musix/engine/internal/logger"
)

var (
	floodWaitRegex = regexp.MustCompile(`FLOOD_(?:PREMIUM_)?WAIT_(\d+)`)
	log            = logger.New("telegram")
)

// ParseFloodWait checks if an error is a Telegram FLOOD_WAIT error and returns the wait duration.
func ParseFloodWait(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	matches := floodWaitRegex.FindStringSubmatch(err.Error())
	if len(matches) == 2 {
		seconds, parseErr := strconv.Atoi(matches[1])
		if parseErr == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
	}
	return 0, false
}

// SleepWithContext sleeps for the specified duration or until context cancellation.
func SleepWithContext(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RedactSecret replaces sensitive tokens and hashes with a safe prefix.
func RedactSecret(secret string) string {
	if len(secret) <= 8 {
		return "[REDACTED]"
	}
	return fmt.Sprintf("%s...[REDACTED]", secret[:4])
}
