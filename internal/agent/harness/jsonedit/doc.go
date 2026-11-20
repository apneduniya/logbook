// Package jsonedit edits JSON objects without disturbing what it doesn't
// touch. It keeps the order of keys and the exact bytes of every value it
// isn't asked to change, so a harness's settings file keeps the user's
// layout and unknown keys when Logbook adds or removes its hook entries.
//
// It also reads JSONC (comments and trailing commas), which some harnesses
// accept in their settings files. It doesn't know about hooks or files; see
// package hookfile for that.
package jsonedit
