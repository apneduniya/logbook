// Package live pushes "your minutes changed" notifications to browsers over
// Server-Sent Events. Hub receives MinutesChanged calls from ingest after
// each commit and fans them out without blocking: a slow client's pending
// notifications are coalesced into one covering range, never queued.
//
// Streams carry no stats themselves; clients refetch the summary they show.
// The owner's stream (/api/v1/me/live) includes the changed time range; a
// public profile stream (/api/v1/users/{username}/live) carries no data at
// all and is closed as soon as the profile is made private. Live does not
// authenticate (handlers take middleware) and does not compute aggregates.
package live
