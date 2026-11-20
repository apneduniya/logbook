package stats

// StreakMinSeconds is the credited time a local day needs to count towards a streak.
const StreakMinSeconds = 15 * 60

// Streak is the user's run of consecutive qualifying local days.
type Streak struct {
	// Current counts back from today, or from yesterday when today does not
	// qualify yet, so a streak is not broken before the day is over.
	Current int `json:"current"`
	Longest int `json:"longest"`
}

// computeStreak derives the streak from qualifying days sorted ascending,
// without duplicates. Days after today (clock skew) are ignored.
func computeStreak(days []Date, today Date) Streak {
	var s Streak
	run := 0
	var prev Date
	for i, d := range days {
		if today.Before(d) {
			break
		}
		if i > 0 && prev.AddDays(1).Equal(d) {
			run++
		} else {
			run = 1
		}
		prev = d
		s.Longest = max(s.Longest, run)
	}
	if run > 0 && (prev.Equal(today) || prev.Equal(today.AddDays(-1))) {
		s.Current = run
	}
	return s
}
