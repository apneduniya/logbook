// Package projects lists the signed-in user's projects with their credited
// totals and lets the user change per-project settings (GitHub repo link,
// hidden, archived), recording every change in the audit log.
//
// A project exists once it has tracked minutes or a settings row. Totals come
// from user_minutes only. Public visibility of projects is enforced by
// package profiles; time reassignment between projects is not handled here.
package projects
