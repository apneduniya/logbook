package live_test

import (
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/profiles"
	"github.com/apneduniya/logbook/internal/server/stats"
)

// publisher mirrors ingest.Publisher; ingest calls Hub through it.
type publisher interface {
	MinutesChanged(userID uuid.UUID, from, to time.Time)
}

// Compile-time checks of the contracts other packages rely on.
var (
	_ publisher                = (*live.Hub)(nil)
	_ publisher                = live.Fanout{}
	_ live.MinutesListener     = (*stats.Service)(nil)
	_ profiles.PrivacyListener = (*live.Hub)(nil)
	_ live.PublicProfiles      = (*profiles.Service)(nil)
)
