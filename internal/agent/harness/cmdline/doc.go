// Package cmdline builds the hook command line Logbook writes into a
// harness's config, quoted for the shell that harness runs it with, and
// recognises Logbook's own command lines when reading a config back.
//
// A Logbook hook command is always "<binPath> hook <harness> <event>". The
// installer uses that shape (not a marker key) to find and remove only its own
// entries, so it never touches hooks the user or other tools added.
package cmdline
