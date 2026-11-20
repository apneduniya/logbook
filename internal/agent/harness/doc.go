// Package harness lists the AI harnesses Logbook supports and gives the
// install command and the hook command one place to find them: All returns
// an installer per harness, and Lookup returns the payload parser that
// `logbook hook <harness> <event>` uses. Each harness lives in its own
// subpackage; shared mechanics are in aievent, cmdline, hookfile, hookrun,
// hostenv, jsonedit and localpost.
package harness
