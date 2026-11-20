package live

import (
	"time"

	"github.com/google/uuid"
)

// MinutesListener is told which of a user's minutes changed. It has the
// same method as ingest.Publisher; Hub and stats.Service implement it.
type MinutesListener interface {
	MinutesChanged(userID uuid.UUID, from, to time.Time)
}

// Fanout passes each change to every listener in order. Ingest takes one
// Publisher; wire it a Fanout of the caches to invalidate followed by the
// Hub, so caches are fresh before browsers are told to refetch.
type Fanout []MinutesListener

// MinutesChanged calls MinutesChanged on each listener.
func (f Fanout) MinutesChanged(userID uuid.UUID, from, to time.Time) {
	for _, l := range f {
		l.MinutesChanged(userID, from, to)
	}
}
