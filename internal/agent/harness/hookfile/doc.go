// Package hookfile installs and removes Logbook's hook entries in a
// harness's JSON hook config file, the one shape five of the six supported
// harnesses share. A harness package describes its file (path, layout, the
// entries to add, how to read a command out of an entry) in a Spec; Installer
// does the rest:
//
//   - parse the file (JSON or JSONC), keeping key order and every value it
//     doesn't own;
//   - add or replace only Logbook's entries, recognised by their command
//     line "<binPath> hook <harness> <event>";
//   - skip the write when nothing changed, so re-installing is a no-op;
//   - back up the original file into the Logbook data dir before the first
//     change, and write atomically, following symlinks so dotfile setups
//     keep working;
//   - on uninstall, remove only Logbook's entries and delete the file only if
//     Logbook created it and nothing else is left in it.
//
// It never restores a backup automatically: by uninstall time the user may
// have changed the file, and removing only our entries keeps those changes.
package hookfile
