// Package atomicfile writes files so that readers see either the old or the
// new content, never a partial write. The installer uses it for every file it
// creates or rewrites in the user's home directory. It does not decide what
// to write or where.
package atomicfile
