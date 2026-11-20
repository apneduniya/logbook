// Package aievent turns what a harness parser extracted from a hook payload
// into a wire.AIEvent: it stamps the time, derives the project from the
// working directory, and trims every field to the wire limits. It also reads
// the hook's stdin with a size cap and a time limit, and salvages the small
// top-level fields of a payload too big to read whole.
//
// Parsers hand it metadata only. Nothing here ever sees prompt text or tool
// input or output except as a length (ADR 0009).
package aievent
