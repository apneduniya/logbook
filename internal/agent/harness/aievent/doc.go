// Package aievent turns what a harness parser extracted from a hook payload
// into a wire.AIEvent: it stamps the time, derives the project from the
// working directory, and trims every field to the wire limits. It also reads
// the hook's stdin with a size cap and a time limit, and salvages the small
// top-level fields of a payload too big to read whole.
//
// Parsers hand it metadata only. Prompt text is seen only inside
// Prompt.UnmarshalJSON, which keeps the prompt's length and the SHA-256 of
// its normalised form (NormalizePrompt) and drops the text; the hook then
// keys that digest into wire.AIEvent.PromptHash (PromptDigest.Keyed). Tool
// input and output are never read (ADR 0009).
package aievent
