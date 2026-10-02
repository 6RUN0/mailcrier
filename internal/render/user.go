package render

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"text/template"
	tparse "text/template/parse"
	"time"

	"github.com/6RUN0/slendmail/internal/text"
)

// Bounds of a template from the configuration: one execution, and all
// the executions that fit the text of one message for one target, which
// Fit repeats dozens of times for a long text. They keep a heavy template
// on a big message from stalling the delivery or filling the memory; they
// do not guard against the author of the template, who is root.
const (
	userTimeout   = time.Second
	userBudget    = 2 * time.Second
	userMaxOutput = 1 << 20
)

// TemplateError is a failure of a template from the configuration: its
// execution failed, panicked, ran past the time or output limit, or wrote
// nothing but white space, or Fit found no rendering within the limit of
// a strict format. The caller renders the message with the built-in
// template of the target instead.
type TemplateError struct {
	// Err is the cause; ErrLimitTooSmall among others.
	Err error
}

// Error returns the cause behind a prefix that names the template kind.
func (e *TemplateError) Error() string { return "user template: " + e.Err.Error() }

// Unwrap returns the cause.
func (e *TemplateError) Unwrap() error { return e.Err }

// Causes of a TemplateError besides the errors of text/template.
var (
	errTimeout     = errors.New("execution exceeded the time limit")
	errBudgetSpent = errors.New("time budget of the text spent before this execution")
	errBroken      = errors.New("disabled for the rest of the process after exceeding the time limit")
	errOutputLimit = errors.New("output exceeds the size limit")
	errEmpty       = errors.New("output is empty")
)

// userLimits bound the executions of one template from the configuration.
type userLimits struct {
	timeout   time.Duration
	budget    time.Duration
	maxOutput int
	// now is the clock of the budget, which tests advance by hand.
	now func() time.Time
	// isBroken is set by the first execution that runs out of time. The
	// template then fails at once for the rest of the process, instead of
	// leaving one more goroutine behind per message; the goroutine left
	// behind stops at its next write, which sees the flag.
	isBroken atomic.Bool
	// running counts the executions in progress, those left behind by a
	// timeout included, so that a test can wait for them to end.
	running sync.WaitGroup
}

// Parse parses source, a template from the configuration, for a target
// whose markup is format: Fit cuts its output as strictly as that of the
// built-in template of format. define, block and template are rejected,
// so that a template cannot call itself; the error then gives the
// position, as a syntax error of text/template does.
func Parse(name, source string, format text.Format) (*Template, error) {
	return parseUser(name, source, format, nil)
}

// parseUser is Parse with extra functions, which tests use to block an
// execution.
func parseUser(name, source string, format text.Format, extra template.FuncMap) (*Template, error) {
	tmpl, err := template.New(name).Funcs(funcs()).Funcs(extra).Parse(source)
	if err != nil {
		return nil, err
	}
	if err := rejectNesting(tmpl); err != nil {
		return nil, err
	}
	return &Template{tmpl: tmpl, format: format, user: &userLimits{timeout: userTimeout, budget: userBudget, maxOutput: userMaxOutput, now: time.Now}}, nil
}

// WithBudget returns t bounded, over all its executions, by its budget
// from now, or by deadline when that is earlier and not zero: the copy
// renders the text of one message for one target. A built-in template has
// no bounds and is returned as it is. An execution that the end of the
// budget cuts short fails as one past the time limit does; one that would
// start after it fails without running, and the template stays usable.
func (t *Template) WithBudget(deadline time.Time) *Template {
	if t.user == nil {
		return t
	}
	bounded := *t
	bounded.deadline = t.user.now().Add(t.user.budget)
	if !deadline.IsZero() && deadline.Before(bounded.deadline) {
		bounded.deadline = deadline
	}
	return &bounded
}

// rejectNesting fails for a template node, which {{template}} and
// {{block}} leave in the tree, and for any template defined besides the
// main one. A define that only replaces an empty main template under its
// own name leaves neither and is let through: without a template node it
// can call nothing.
func rejectNesting(tmpl *template.Template) error {
	if tmpl.Tree != nil {
		if node := findTemplateNode(tmpl.Root); node != nil {
			location, _ := tmpl.ErrorContext(node)
			return fmt.Errorf("template: %s: {{template}} and {{block}} are not allowed", location)
		}
	}
	var defined []*template.Template
	for _, other := range tmpl.Templates() {
		if other.Name() != tmpl.Name() && other.Tree != nil {
			defined = append(defined, other)
		}
	}
	if len(defined) == 0 {
		return nil
	}
	first := slices.MinFunc(defined, func(a, b *template.Template) int { return int(a.Root.Pos - b.Root.Pos) })
	location, _ := first.ErrorContext(first.Root)
	return fmt.Errorf("template: %s: {{define}} is not allowed", location)
}

// findTemplateNode returns the first template node under node, nil when
// there is none. Only lists and the bodies of if, range and with hold
// other nodes; pipelines hold no template node.
func findTemplateNode(node tparse.Node) tparse.Node {
	switch n := node.(type) {
	case *tparse.TemplateNode:
		return n
	case *tparse.ListNode:
		if n == nil {
			return nil
		}
		for _, child := range n.Nodes {
			if found := findTemplateNode(child); found != nil {
				return found
			}
		}
	case *tparse.IfNode:
		return findInBranch(&n.BranchNode)
	case *tparse.RangeNode:
		return findInBranch(&n.BranchNode)
	case *tparse.WithNode:
		return findInBranch(&n.BranchNode)
	}
	return nil
}

// findInBranch returns the first template node in the bodies of branch.
func findInBranch(branch *tparse.BranchNode) tparse.Node {
	if found := findTemplateNode(branch.List); found != nil {
		return found
	}
	return findTemplateNode(branch.ElseList)
}

// executeUser renders d with a template from the configuration in a
// goroutine of its own, so that a timeout, or the end of the budget set
// by WithBudget, can give up on it, with the
// output bounded by a writer that fails past the limit, which ends the
// execution. A panic in the goroutine becomes an error.
func (t *Template) executeUser(d Data) ([]byte, error) {
	limits := t.user
	if limits.isBroken.Load() {
		return nil, &TemplateError{Err: errBroken}
	}
	wait := limits.timeout
	if !t.deadline.IsZero() {
		wait = min(wait, t.deadline.Sub(limits.now()))
	}
	// No goroutine is left behind here, and the budget may have gone on
	// other templates or on the queue run: the template stays usable.
	if wait <= 0 {
		return nil, &TemplateError{Err: errBudgetSpent}
	}
	type outcome struct {
		out []byte
		err error
	}
	done := make(chan outcome, 1)
	limits.running.Go(func() {
		defer func() {
			if value := recover(); value != nil {
				done <- outcome{err: fmt.Errorf("panic: %v", value)}
			}
		}()
		w := &limitedWriter{max: limits.maxOutput, isBroken: &limits.isBroken}
		err := t.tmpl.Execute(w, d)
		done <- outcome{out: w.buf.Bytes(), err: err}
	})
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-done:
		switch {
		case result.err != nil:
			return nil, &TemplateError{Err: result.err}
		case len(bytes.TrimSpace(result.out)) == 0:
			return nil, &TemplateError{Err: errEmpty}
		}
		return result.out, nil
	case <-timer.C:
		limits.isBroken.Store(true)
		return nil, &TemplateError{Err: errTimeout}
	}
}

// limitedWriter collects the output of one execution up to max bytes. A
// write past max, or after the template was marked broken, fails, and
// text/template ends the execution with that error.
type limitedWriter struct {
	buf      bytes.Buffer
	max      int
	isBroken *atomic.Bool
}

// Write appends p, or fails without writing anything.
func (w *limitedWriter) Write(p []byte) (int, error) {
	switch {
	case w.isBroken.Load():
		return 0, errBroken
	case w.buf.Len()+len(p) > w.max:
		return 0, errOutputLimit
	}
	return w.buf.Write(p)
}
