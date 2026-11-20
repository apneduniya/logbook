// Package profiles serves public user profiles (/api/v1/users/{username}/profile)
// and the signed-in user's own profile settings (/api/v1/me/profile).
//
// Public profiles show only what the user chose to publish: never email or
// HCA id, never hidden projects or their time, and nothing at all when
// profile_public is false (the response is then the same 404 as for an
// unknown username, so profiles can't be enumerated). Profile changes are
// audited. Sign-in and account creation belong to package auth.
package profiles
