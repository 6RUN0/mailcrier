// Command check-refs fails when commit messages, code comments or
// documentation changed since a base revision refer to plan stages or to
// review and decision IDs. Such references point into working notes that are
// not part of the repository, so the reason for a change has to be written
// out in words instead.
//
// Usage:
//
//	go run ./scripts/check-refs -base REV
//
// It checks the messages of the commits in REV..HEAD and the files that
// differ from REV in the working tree, untracked files included. Findings go
// to stdout, one per line; the exit status is 1 when there are findings and
// 2 when the check itself fails.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// commonRefs are forbidden everywhere: review IDs, decision IDs, exit code
// matrix rows and plan stages.
var commonRefs = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:C|N|R3|R5)-[BSMGQm]\d+\b`),
	regexp.MustCompile(`\bA-B?\d+\b`),
	regexp.MustCompile(`\bU-\d+\b`),
	regexp.MustCompile(`\bX-\d{2}\b`),
	regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])((?:этап\p{L}*|stage)\s\d+)\b`),
}

// decisionRefs look like ordinary tokens (a letter and a number) in prose,
// so they are only forbidden in commit messages and code comments. A
// preceding dash is allowed so that compiler flags such as -O1 pass.
var decisionRefs = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|[^\w-])([OP]\d{1,2})\b`),
}

// manPageExt matches the section suffix of a man page source, such as ".8".
var manPageExt = regexp.MustCompile(`^\.[1-9]$`)

// textKind selects the patterns that apply to a piece of text.
type textKind int

const (
	// commentText is a commit message or a code comment.
	commentText textKind = iota
	// documentText is prose meant for readers: Markdown, man pages, tables.
	documentText
)

// finding is one forbidden reference.
type finding struct {
	where string
	line  int
	ref   string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: forbidden reference %q", f.where, f.line, f.ref)
}

func main() {
	base := flag.String("base", "", "base revision; commits in base..HEAD and files changed since base are checked")
	flag.Parse()
	if *base == "" {
		flag.Usage()
		os.Exit(2)
	}
	findings, err := check(*base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-refs:", err)
		os.Exit(2)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}

func check(base string) ([]finding, error) {
	var findings []finding
	messages, err := git("log", "--no-merges", "--format=%H%x00%ae%x00%B%x1e", base+"..HEAD")
	if err != nil {
		return nil, err
	}
	for _, record := range strings.Split(messages, "\x1e") {
		fields := strings.SplitN(strings.TrimLeft(record, "\n"), "\x00", 3)
		if len(fields) == 3 {
			findings = append(findings, findRefs("commit "+fields[0][:12], checkedMessage(fields[1], fields[2]), commentText)...)
		}
	}
	// -z keeps names with spaces or non-ASCII bytes unquoted.
	changed, err := git("diff", "-z", "--name-only", "--diff-filter=d", base)
	if err != nil {
		return nil, err
	}
	untracked, err := git("ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, file := range strings.Split(changed+untracked, "\x00") {
		if file == "" {
			continue
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		fileFindings, err := checkFile(file, content)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fileFindings...)
	}
	return findings, nil
}

// dependabotEmail is the author email of Dependabot commits; unlike the
// author name it cannot be borrowed with git config.
const dependabotEmail = "49699333+dependabot[bot]@users.noreply.github.com"

// checkedMessage returns the part of a commit message the author controls.
// Dependabot bodies quote release notes of other projects, so only its
// subject is checked.
func checkedMessage(email, message string) string {
	if email == dependabotEmail {
		subject, _, _ := strings.Cut(message, "\n")
		return subject
	}
	return message
}

func git(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// checkFile checks the comments of code and configuration files and the
// whole text of documentation. Other files, such as test fixtures, are data
// and are not checked.
func checkFile(file string, content []byte) ([]finding, error) {
	name := filepath.Base(file)
	switch ext := filepath.Ext(name); {
	case ext == ".go":
		return goCommentRefs(file, content)
	case ext == ".md" || ext == ".tsv" || manPageExt.MatchString(ext):
		return findRefs(file, string(content), documentText), nil
	case ext == ".yml" || ext == ".yaml" || ext == ".toml" || ext == ".sh" || name == "Makefile" || name == ".gitignore":
		return hashCommentRefs(file, string(content)), nil
	default:
		return nil, nil
	}
}

func goCommentRefs(file string, src []byte) ([]finding, error) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var findings []finding
	for _, group := range parsed.Comments {
		for _, comment := range group.List {
			line := fset.Position(comment.Pos()).Line
			for _, f := range findRefs(file, comment.Text, commentText) {
				f.line += line - 1
				findings = append(findings, f)
			}
		}
	}
	return findings, nil
}

// hashCommentRefs checks the text after the first "#" of each line; a "#"
// inside a quoted value can only add findings, never hide one.
func hashCommentRefs(file, content string) []finding {
	var findings []finding
	for i, line := range strings.Split(content, "\n") {
		if _, comment, ok := strings.Cut(line, "#"); ok {
			for _, f := range findRefs(file, comment, commentText) {
				f.line = i + 1
				findings = append(findings, f)
			}
		}
	}
	return findings
}

// findRefs returns the forbidden references in text; line numbers start at 1.
func findRefs(where, text string, kind textKind) []finding {
	patterns := commonRefs
	if kind == commentText {
		patterns = append(append([]*regexp.Regexp{}, commonRefs...), decisionRefs...)
	}
	var findings []finding
	for i, line := range strings.Split(text, "\n") {
		for _, pattern := range patterns {
			for _, match := range pattern.FindAllStringSubmatch(line, -1) {
				ref := match[0]
				if len(match) > 1 {
					ref = match[1]
				}
				findings = append(findings, finding{where: where, line: i + 1, ref: ref})
			}
		}
	}
	return findings
}
