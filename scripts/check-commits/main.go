// Command check-commits fails when a commit in REV..HEAD does not follow
// Conventional Commits: the changelog is generated from commit subjects, and
// a malformed subject drops the change from it.
//
// Usage:
//
//	go run ./scripts/check-commits -base REV
//
// Merge commits are skipped. Commits by Dependabot are exempt from the
// length limit only: its subjects embed module paths and directories and
// cannot be shortened by configuration. Findings go to stdout, one per line; the exit
// status is 1 when there are findings and 2 when the check itself fails.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// maxSubjectLength keeps subjects readable in one line of git log and of
// the generated changelog.
const maxSubjectLength = 72

// dependabotEmail is the author email of Dependabot commits. The email, not
// the name, identifies them: a name is set freely with git config, while
// this noreply address belongs to the Dependabot account.
const dependabotEmail = "49699333+dependabot[bot]@users.noreply.github.com"

var subjectFormat = regexp.MustCompile(`^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9._/-]+\))?!?: \S`)

func main() {
	base := flag.String("base", "", "base revision; commits in base..HEAD are checked")
	flag.Parse()
	if *base == "" {
		flag.Usage()
		os.Exit(2)
	}
	log, err := gitLog(*base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-commits:", err)
		os.Exit(2)
	}
	failed := false
	for _, record := range strings.Split(log, "\x1e") {
		fields := strings.SplitN(strings.TrimLeft(record, "\n"), "\x00", 3)
		if len(fields) != 3 {
			continue
		}
		hash, email, message := fields[0], fields[1], fields[2]
		for _, problem := range checkMessage(email, message) {
			fmt.Printf("commit %s: %s\n", hash[:12], problem)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func gitLog(base string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "log", "--no-merges", "--format=%H%x00%ae%x00%B%x1e", base+"..HEAD")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// checkMessage returns the problems of one commit message by the author
// with the given email.
func checkMessage(email, message string) []string {
	lines := strings.Split(strings.TrimRight(message, "\n"), "\n")
	subject := lines[0]
	var problems []string
	if !subjectFormat.MatchString(subject) {
		problems = append(problems, fmt.Sprintf("subject %q is not \"type(scope): description\" with a Conventional Commits type", subject))
	}
	if n := len([]rune(subject)); n > maxSubjectLength && email != dependabotEmail {
		problems = append(problems, fmt.Sprintf("subject is %d characters, limit %d", n, maxSubjectLength))
	}
	if strings.HasSuffix(subject, ".") {
		problems = append(problems, "subject ends with a period")
	}
	if len(lines) > 1 && lines[1] != "" {
		problems = append(problems, "no blank line between subject and body")
	}
	return problems
}
