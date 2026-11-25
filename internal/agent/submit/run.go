package submit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/apneduniya/logbook/internal/agent/crediting"
	"github.com/apneduniya/logbook/internal/agent/install/atomicfile"
	"github.com/apneduniya/logbook/internal/agent/serverapi"
	"github.com/apneduniya/logbook/pkg/projectname"
	"github.com/apneduniya/logbook/pkg/replay"
	"github.com/apneduniya/logbook/pkg/submission"
)

// Errors Run returns before anything is uploaded.
var (
	// ErrToday: the range doesn't end before today (UTC).
	ErrToday = errors.New("today's evidence is sealed after midnight UTC")
	// ErrNotSealed: a device with rows in the range hasn't sealed its end.
	ErrNotSealed = errors.New("not every device has sealed the range yet")
	// ErrNoTTY: confirmation is needed but stdin isn't a terminal.
	ErrNoTTY = errors.New("stdin isn't a terminal, so there's no way to confirm the upload; " +
		"check it with --preview FILE, then run again with --yes")
	// ErrCancelled: the user didn't confirm.
	ErrCancelled = errors.New("cancelled; nothing was uploaded")
	// ErrNothing: there is nothing to submit or add.
	ErrNothing = errors.New("nothing to submit")
)

// Request is what the user asked for.
type Request struct {
	Project  string
	From, To string // YYYY-MM-DD, inclusive
	// Program is a program's slug, "" for none. Not with AddTo.
	Program string
	// AddTo is an existing submission's id: upload this device's part of
	// it. Project and range must be the submission's.
	AddTo string
	// Yes skips the confirmation.
	Yes bool
	// Preview, when set, is a file to write the request body to instead of
	// uploading it. It is replaced, never written in place, with mode 0600.
	Preview string
}

// Server is the server calls Run makes. *serverapi.Client satisfies it.
type Server interface {
	Plan(ctx context.Context, t serverapi.Target, q serverapi.PlanQuery) (submission.Plan, error)
	Submit(ctx context.Context, t serverapi.Target, body []byte) (serverapi.Submitted, error)
	AddPart(ctx context.Context, t serverapi.Target, id string, body []byte) (serverapi.Submitted, error)
}

// Prover builds proofs for this device's keys; crediting.BuildProofs over
// the local log.
type Prover func(ctx context.Context, keys []replay.Key) (crediting.Proofs, error)

// Env is what Run works with.
type Env struct {
	Server   Server
	Target   serverapi.Target
	DeviceID string
	Prove    Prover
	// In is read for the confirmation when Interactive (stdin is a
	// terminal); Out gets the summary and the result.
	In          io.Reader
	Out         io.Writer
	Interactive bool
	Now         func() time.Time
}

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// check validates the request and refuses a range that ends today or
// later.
func (r *Request) check(now time.Time) error {
	r.Project = projectname.Normalize(r.Project)
	if r.Project == "" || len(r.Project) > 256 {
		return errors.New("name the project to submit, as Logbook shows it")
	}
	if _, err := submission.ParseRange(r.From, r.To); err != nil {
		return fmt.Errorf("--from %s --to %s: %s", r.From, r.To, rangeProblem(err))
	}
	if len(r.Program) > 64 {
		return errors.New("--program is a program's slug")
	}
	if r.AddTo != "" {
		r.AddTo = strings.ToLower(r.AddTo)
		if !idRe.MatchString(r.AddTo) {
			return fmt.Errorf("--add %q is not a submission id", r.AddTo)
		}
		if r.Program != "" {
			return errors.New("--program can't be used with --add: the submission already names its program")
		}
	}
	today := now.UTC().Format(time.DateOnly)
	if r.To >= today {
		last := now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
		return fmt.Errorf("the range ends %s, but %w; end it on %s or earlier", r.To, ErrToday, last)
	}
	return nil
}

// rangeProblem is why submission.ParseRange refused a range, in the
// flags' words.
func rangeProblem(err error) string {
	msg := strings.TrimPrefix(err.Error(), submission.ErrMalformed.Error()+": ")
	switch {
	case msg == "to is before from":
		return "--to is before --from"
	case strings.HasPrefix(msg, "from "), strings.HasPrefix(msg, "to "):
		return "--" + msg + " (days are YYYY-MM-DD, UTC)"
	case strings.HasSuffix(msg, fmt.Sprintf("at most %d", submission.MaxRangeDays)):
		return fmt.Sprintf("the range is %s; submit at most %d days at a time", strings.TrimSuffix(msg, fmt.Sprintf(", at most %d", submission.MaxRangeDays)), submission.MaxRangeDays)
	}
	return msg
}

// Run submits the device's part (ADR 0020 "The bundle"): it fetches the
// plan, builds the part, prints the summary, asks for confirmation, and
// uploads the part gzip-compressed, or writes it to req.Preview. With
// req.AddTo the plan is the submission's own frozen rows, so the part
// lists none the server didn't freeze.
func Run(ctx context.Context, req Request, env Env) error {
	if env.Now == nil {
		env.Now = time.Now
	}
	if err := req.check(env.Now()); err != nil {
		return err
	}
	if !req.Yes && req.Preview == "" && !env.Interactive {
		return ErrNoTTY
	}
	plan, err := env.Server.Plan(ctx, env.Target, serverapi.PlanQuery{
		Project: req.Project, From: req.From, To: req.To, DeviceID: env.DeviceID, Submission: req.AddTo,
	})
	if errors.Is(err, serverapi.ErrRefused) {
		return fmt.Errorf("the server can't take this range: %w", err)
	}
	if req.AddTo != "" && (errors.Is(err, serverapi.ErrNotFound) || errors.Is(err, serverapi.ErrConflict)) {
		return fmt.Errorf("submission %s: %w", req.AddTo, err)
	}
	if err != nil {
		return fmt.Errorf("asking the server which minutes it holds: %w", err)
	}
	if err := sealed(plan, req.To); err != nil {
		return err
	}
	if len(plan.Minutes) == 0 {
		return fmt.Errorf("%w: the server holds no minutes of %q from %s to %s", ErrNothing, req.Project, req.From, req.To)
	}
	var proofs crediting.Proofs
	if keys := Keys(plan, env.DeviceID); len(keys) > 0 {
		if proofs, err = env.Prove(ctx, keys); err != nil {
			return err
		}
	}
	prep, err := Prepare(plan, env.DeviceID, proofs)
	if err != nil {
		return err
	}
	if req.AddTo != "" && len(prep.Part.Minutes) == 0 {
		writeSummary(env.Out, req, prep.Summary)
		return fmt.Errorf("%w: this device can back none of the submission's minutes", ErrNothing)
	}
	body, err := encode(req, prep.Part)
	if err != nil {
		return err
	}
	writeSummary(env.Out, req, prep.Summary)

	if req.Preview != "" {
		// A temporary file (0600) renamed over the target: os.WriteFile
		// keeps an existing file's mode, which may let other local users
		// read the revealed evidence.
		if err := atomicfile.Write(req.Preview, body, 0o600); err != nil {
			return fmt.Errorf("writing the preview: %w", err)
		}
		return say(env.Out, "\nWrote the request body (%d bytes of JSON) to %s. Nothing was uploaded.\n", len(body), req.Preview)
	}
	if !req.Yes {
		ok, err := confirm(env.In, env.Out, env.Target.BaseURL)
		if err != nil {
			return err
		}
		if !ok {
			return ErrCancelled
		}
	}
	var res serverapi.Submitted
	if req.AddTo != "" {
		res, err = env.Server.AddPart(ctx, env.Target, req.AddTo, body)
	} else {
		res, err = env.Server.Submit(ctx, env.Target, body)
	}
	if err != nil {
		return fmt.Errorf("uploading: %w", err)
	}
	return writeResult(env.Out, req, prep.Summary, res)
}

// sealed refuses a range that an unrevoked device with rows hasn't sealed
// through its end. The server refuses it too (400); this says which
// device. A revoked device can't seal more days, so it doesn't count.
func sealed(plan submission.Plan, to string) error {
	const retired = "; if it's no longer in use (lost, replaced or reinstalled), revoke it on your Logbook dashboard " +
		"and its minutes there stay unbacked"
	for _, d := range plan.Devices {
		switch {
		case d.Revoked:
		case d.SealedThrough == nil:
			return fmt.Errorf("%w: %s has sealed no day yet; a day is sealed about an hour after it ends (UTC) while the agent runs there%s",
				ErrNotSealed, d.Name, retired)
		case *d.SealedThrough < to:
			return fmt.Errorf("%w: %s has sealed its evidence through %s; end the range there, or wait until it seals %s (about an hour after the day ends, UTC, while the agent runs there)%s",
				ErrNotSealed, d.Name, *d.SealedThrough, to, retired)
		}
	}
	return nil
}

// encode returns the request body: the upload, or for --add the part.
func encode(req Request, part submission.Part) ([]byte, error) {
	var v interface{ Validate() error } = part
	if req.AddTo == "" {
		v = submission.Upload{Project: req.Project, From: req.From, To: req.To, Program: req.Program, Part: part}
	}
	if err := v.Validate(); err != nil {
		return nil, fmt.Errorf("building the upload: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding the upload: %w", err)
	}
	return b, nil
}

// confirm asks whether to upload; only "y" or "yes" says yes, and end of
// input says no.
func confirm(in io.Reader, out io.Writer, server string) (bool, error) {
	if err := say(out, "\nUpload this to %s? [y/N] ", server); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(io.LimitReader(in, 1024)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading the answer: %w", err)
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

func writeResult(out io.Writer, req Request, s Summary, res serverapi.Submitted) error {
	var b strings.Builder
	if req.AddTo != "" {
		fmt.Fprintf(&b, "\nAdded this device's part to submission %s", res.ID)
	} else {
		fmt.Fprintf(&b, "\nSubmitted %s, %s to %s, as submission %s", req.Project, req.From, req.To, res.ID)
	}
	if res.Status != "" {
		fmt.Fprintf(&b, " (%s: the server re-checks it in the background)", res.Status)
	}
	b.WriteString(".\n")
	if res.ReviewURL != "" {
		fmt.Fprintf(&b, "Review link: %s\n", res.ReviewURL)
	} else {
		b.WriteString("Its review link is on your Logbook dashboard, under Submissions.\n")
	}
	if req.AddTo == "" && len(s.Others) > 0 {
		fmt.Fprintf(&b, "Minutes from your other devices show as not yet backed by evidence until each adds its part:\n"+
			"  logbook submit %s --from %s --to %s --add %s\n", quoteArg(req.Project), req.From, req.To, res.ID)
	}
	return say(out, "%s", b.String())
}

// quoteArg quotes a project name that a shell would split.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t'\"$`\\&|;<>()*?!#~") {
		return `"` + strings.NewReplacer(`"`, `\"`, `\`, `\\`, "$", `\$`, "`", "\\`").Replace(s) + `"`
	}
	return s
}

func say(w io.Writer, format string, a ...any) error {
	if _, err := fmt.Fprintf(w, format, a...); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
