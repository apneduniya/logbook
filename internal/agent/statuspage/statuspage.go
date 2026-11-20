package statuspage

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"time"

	"github.com/apneduniya/logbook/internal/agent/status"
)

//go:embed page.html.tmpl
var pageSource string

// Renderer renders the status page. Create it once with New.
type Renderer struct {
	tmpl *template.Template
	now  func() time.Time
}

// New parses the embedded template.
func New(now func() time.Time) (*Renderer, error) {
	if now == nil {
		now = time.Now
	}
	r := &Renderer{now: now}
	t, err := template.New("page").Funcs(template.FuncMap{
		"ago":   r.ago,
		"bytes": humanBytes,
		// unknown tells "the installer didn't report" (nil) from "no hooks".
		"unknown": func(h []status.Hook) bool { return h == nil },
	}).Parse(pageSource)
	if err != nil {
		return nil, fmt.Errorf("parsing status page template: %w", err)
	}
	r.tmpl = t
	return r, nil
}

// Render writes the page for s to w. It renders into a buffer first so a
// template error never leaves a half-written page.
func (r *Renderer) Render(w io.Writer, s status.Snapshot) error {
	var buf bytes.Buffer
	if err := r.tmpl.Execute(&buf, s); err != nil {
		return fmt.Errorf("rendering status page: %w", err)
	}
	if _, err := buf.WriteTo(w); err != nil {
		return fmt.Errorf("writing status page: %w", err)
	}
	return nil
}

// ago formats a past time as "never", "just now", "5 min ago", "3 h ago"
// or a date.
func (r *Renderer) ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := r.now().Sub(t)
	switch {
	case d < 0:
		return "in " + (-d).Round(time.Second).String()
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2 Jan 2006 15:04")
}

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
