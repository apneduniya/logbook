package programapi

// Hackatime's trust levels (User.trust_levels at 43bde10) and the level
// Logbook stores when it can't read one ("unknown", package hackatime).
const (
	levelBlue    = "blue"
	levelRed     = "red"
	levelGreen   = "green"
	levelYellow  = "yellow"
	levelUnknown = "unknown"
)

// trustValues is Hackatime's enum: blue 0, red 1, green 2, yellow 3.
var trustValues = map[string]int{levelBlue: 0, levelRed: 1, levelGreen: 2, levelYellow: 3}

// Trust is a user's trust level as programs see it.
type Trust struct {
	Level string
	Value int
	// Verified is false when the level is a stand-in: the user has no
	// Hackatime link, or Logbook couldn't read the level.
	Verified bool
}

// MaskTrust turns a stored Hackatime trust level into what programs see, as
// Hackatime's public_trust_level does: yellow (suspected) reads as blue.
// linked is false when the user has no Hackatime link; stored is the link's
// level otherwise. An unlinked user, or one whose level is unknown or not a
// Hackatime level, reads as blue (Hackatime's level for an account nobody
// has judged) with Verified false. Red stays red: the trust refresher never
// downgrades it, and a red link can't be removed.
func MaskTrust(stored string, linked bool) Trust {
	if !linked {
		return Trust{Level: levelBlue, Value: trustValues[levelBlue]}
	}
	switch stored {
	case levelYellow:
		return Trust{Level: levelBlue, Value: trustValues[levelBlue], Verified: true}
	case levelBlue, levelRed, levelGreen:
		return Trust{Level: stored, Value: trustValues[stored], Verified: true}
	default: // unknown, or anything unexpected
		return Trust{Level: levelBlue, Value: trustValues[levelBlue]}
	}
}

// Banned reports whether the user is convicted (red) on Hackatime. Hackatime
// treats a red user's own credentials as absent (api_access_restricted?).
func (t Trust) Banned() bool { return t.Level == levelRed }
