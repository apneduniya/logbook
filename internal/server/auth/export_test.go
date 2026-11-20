package auth

// KeyFailBuckets exposes how many clients the failed-API-key limiter tracks,
// for tests in package auth_test.
func KeyFailBuckets(s *Service) int { return s.keyFailLimiter.Len() }
