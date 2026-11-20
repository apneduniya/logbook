// Package useragent extracts the editor, operating system and plugin from the
// User-Agent strings that wakatime-cli and editor plugins send. It is pure
// string parsing with no I/O.
//
// wakatime-cli builds its User-Agent as
//
//	wakatime/<cli version> (<os>-<kernel>-<arch>) <go runtime> <plugin chain>
//
// where the plugin chain is whatever the editor plugin passed with --plugin,
// usually "<editor>/<version> <editor>-wakatime/<version>". The rules follow
// Hackatime's WakatimeUserAgentParser for the formats seen in production; rare
// legacy formats it special-cases are not handled and simply yield an empty
// editor, which the caller fills from the heartbeat body.
package useragent
