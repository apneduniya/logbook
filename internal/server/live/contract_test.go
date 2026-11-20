package live_test

import (
	"time"

	"github.com/google/uuid"

	"github.com/apneduniya/logbook/internal/server/live"
	"github.com/apneduniya/logbook/internal/server/profiles"
)

// publisher mirrors ingest.Publisher; ingest calls Hub through it.
type publisher interface {
	MinutesChanged(userID uuid.UUID, from, to time.Time)
}

// Compile-time checks of the contracts other packages rely on.
var (
	_ publisher                = (*live.Hub)(nil)
	_ profiles.PrivacyListener = (*live.Hub)(nil)
	_ live.PublicLookup        = (*profiles.Service)(nil)
)
