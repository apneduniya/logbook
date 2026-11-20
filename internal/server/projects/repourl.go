package projects

import (
	"errors"
	"regexp"
)

// ErrInvalidRepoURL is returned for repo links that are not a GitHub repository URL.
var ErrInvalidRepoURL = errors.New("repo_url must look like https://github.com/<owner>/<repo>")

// repoURLPattern follows GitHub's naming rules: owners are 1-39 letters,
// digits or hyphens, not starting with a hyphen; repos are 1-100 of letters,
// digits, '.', '-' and '_'. No query, fragment, port, userinfo or trailing slash.
var repoURLPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]{0,38}/([A-Za-z0-9._-]{1,100})$`)

// ValidateRepoURL accepts "" (clears the link) or https://github.com/<owner>/<repo>.
func ValidateRepoURL(s string) error {
	if s == "" {
		return nil
	}
	m := repoURLPattern.FindStringSubmatch(s)
	// "." and ".." would resolve to a different path in a browser.
	if m == nil || m[1] == "." || m[1] == ".." {
		return ErrInvalidRepoURL
	}
	return nil
}
