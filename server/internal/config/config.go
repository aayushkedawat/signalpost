// Package config holds the server's runtime tunables and their defaults.
package config

import (
	"os"
	"path/filepath"
	"time"

	"trafficlight/internal/sessions"
)

// Version is reported by GET /health.
const Version = "0.1.0"

type Config struct {
	// Addr defaults to loopback. LAN exposure (needed once the watch
	// client exists) is an explicit opt-in, per PRD.md §8.
	Addr string

	// TokenPath is where the bearer token is generated/read.
	TokenPath string

	// WaitingTooLong flips ToolStatus.WaitingTooLong for visual urgency.
	// It never decides WAITING itself (PRD.md §5).
	WaitingTooLong time.Duration

	// Sessions carries the DONE window and staleness tuning.
	Sessions sessions.Config
}

func Default() Config {
	return Config{
		Addr:      "127.0.0.1:8787",
		TokenPath: DefaultTokenPath(),
		// Measured against captured sessions: genuine answer times for a
		// permission prompt were 1.8, 6.4, 21.1, 30.3, 56.3, 67.8, 91.6
		// and 602.7 seconds. At the original 30s, half of all ordinary
		// answering would have tripped this, making the "stale" colour
		// routine and therefore meaningless. At 2 minutes only the
		// genuinely abandoned one does.
		WaitingTooLong: 2 * time.Minute,
		Sessions:       sessions.DefaultConfig(),
	}
}

// DefaultTokenPath is ~/.traffic-light/token, falling back to the working
// directory if the home directory cannot be resolved.
func DefaultTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".traffic-light", "token")
	}
	return filepath.Join(home, ".traffic-light", "token")
}
