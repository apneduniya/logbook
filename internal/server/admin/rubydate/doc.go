// Package rubydate reads a date as Ruby's Date.parse does, so the admin
// API's Hackatime-compatible calls accept the same start_date and end_date
// values Hackatime does (DateParsing#parse_ts falls back to Date.parse).
//
// It is a port of the date gem's date__parse (ext/date/date_parse.c, the
// default, non-"tight" parser) and of how Date.parse turns what that finds
// into a date (d_new_by_frags, rt_complete_frags, rt__valid_date_frags_p
// in date_core.c), at the date gem 3.5.1 that Hackatime runs. Only the
// date matters here: times and zones are read so they are taken out of the
// string as Ruby takes them out, then dropped. It owns nothing else and
// does no I/O.
//
// Known differences from Ruby: Ruby's [:alnum:], [:alpha:] and \b treat
// non-ASCII letters as letters, and here every non-ASCII letter or digit
// stands in as "_" (a word character that is neither); numbers past 15
// digits saturate.
package rubydate
