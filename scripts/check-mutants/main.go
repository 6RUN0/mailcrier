// Command check-mutants compares the mutants that survived a run of
// gremlins with a list of accepted ones, so that a new surviving mutant
// fails the run and an accepted one that a test now kills is dropped from
// the list.
//
// Usage:
//
//	go run ./scripts/check-mutants -allow testdata/mutants-allow.txt DIR=REPORT...
//
// Each DIR=REPORT names the directory gremlins ran on, relative to the
// module root, and the JSON file it wrote there with -o; file names in a
// report are relative to its directory. A mutant is keyed by its file, its
// type, its offset in the line without indentation and the text of that
// line, so that it keeps its key when lines above it move.
//
// The list holds one mutant per line, tab-separated: path, type, offset,
// line text, reason. Blank lines and lines starting with # are skipped. A
// mutant LIVED or NOT COVERED needs an entry; an entry needs a mutant of a
// directory of the run that is LIVED, NOT COVERED or TIMED OUT (a run
// under load times out mutants that otherwise live). Entries of
// directories not in the run are not checked. Findings go to stdout, one
// per line; the exit status is 1 when there are findings and 2 when the
// check itself fails.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Statuses of a mutant in a report of gremlins.
const (
	statusLived      = "LIVED"
	statusNotCovered = "NOT COVERED"
	statusTimedOut   = "TIMED OUT"
)

// report is the part of the JSON output of gremlins that the check reads.
type report struct {
	Files []struct {
		FileName  string `json:"file_name"`
		Mutations []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
		} `json:"mutations"`
	} `json:"files"`
}

// key identifies a mutant across edits that only move lines.
type key struct {
	path   string
	kind   string
	offset int
	text   string
}

// String gives the key as a line of the list without its reason.
func (k key) String() string {
	return strings.Join([]string{k.path, k.kind, strconv.Itoa(k.offset), k.text}, "\t")
}

// mutant is one mutant of a run with its position for messages.
type mutant struct {
	key    key
	status string
	line   int
	column int
}

// entry is one accepted mutant of the list.
type entry struct {
	key    key
	lineNo int
}

func main() {
	allowPath := flag.String("allow", "", "file with the accepted mutants")
	flag.Parse()
	if *allowPath == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: check-mutants -allow FILE DIR=REPORT...")
		os.Exit(2)
	}
	findings, err := check(".", *allowPath, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-mutants:", err)
		os.Exit(2)
	}
	for _, finding := range findings {
		fmt.Println(finding)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}

// check reads the reports of runs below root and the list, and returns
// the findings sorted.
func check(root, allowPath string, runs []string) ([]string, error) {
	entries, err := readAllow(allowPath)
	if err != nil {
		return nil, err
	}
	accepted := map[key]bool{}
	for _, e := range entries {
		accepted[e.key] = true
	}
	var findings []string
	seen := map[key][]string{}
	var dirs []string
	for _, run := range runs {
		dir, reportPath, ok := strings.Cut(run, "=")
		if !ok || dir == "" || reportPath == "" {
			return nil, fmt.Errorf("%q is not DIR=REPORT", run)
		}
		dir = path.Clean(filepath.ToSlash(dir))
		dirs = append(dirs, dir)
		mutants, err := readReport(root, dir, reportPath)
		if err != nil {
			return nil, err
		}
		for _, m := range mutants {
			seen[m.key] = append(seen[m.key], m.status)
			if (m.status == statusLived || m.status == statusNotCovered) && !accepted[m.key] {
				findings = append(findings, fmt.Sprintf("%s:%d:%d: %s %s, kill it with a test or list it with a reason:\n\t%s",
					m.key.path, m.line, m.column, m.key.kind, strings.ToLower(m.status), m.key))
			}
		}
	}
	for _, e := range entries {
		if !inRun(e.key.path, dirs) {
			continue
		}
		statuses := seen[e.key]
		switch {
		case len(statuses) == 0:
			findings = append(findings, fmt.Sprintf("%s:%d: no such mutant any more, its line changed or went away: %s",
				allowPath, e.lineNo, e.key))
		case !hasStatus(statuses, statusLived, statusNotCovered, statusTimedOut):
			findings = append(findings, fmt.Sprintf("%s:%d: %s now, remove the entry: %s",
				allowPath, e.lineNo, strings.ToLower(statuses[0]), e.key))
		}
	}
	sort.Strings(findings)
	return findings, nil
}

// readReport reads a report of gremlins for dir and keys its mutants with
// the lines of the source files.
func readReport(root, dir, reportPath string) ([]mutant, error) {
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return nil, err
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", reportPath, err)
	}
	var mutants []mutant
	for _, file := range r.Files {
		filePath := path.Join(dir, filepath.ToSlash(file.FileName))
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(filePath)))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(source), "\n")
		for _, m := range file.Mutations {
			k, err := keyOf(lines, filePath, m.Type, m.Line, m.Column)
			if err != nil {
				return nil, err
			}
			mutants = append(mutants, mutant{key: k, status: m.Status, line: m.Line, column: m.Column})
		}
	}
	return mutants, nil
}

// keyOf builds the key of a mutant at a 1-based line and byte column.
func keyOf(lines []string, filePath, kind string, line, column int) (key, error) {
	if line < 1 || line > len(lines) {
		return key{}, fmt.Errorf("%s:%d: line out of range", filePath, line)
	}
	text := strings.TrimRight(lines[line-1], " \t\r")
	trimmed := strings.TrimLeft(text, " \t")
	offset := column - 1 - (len(text) - len(trimmed))
	if offset < 0 || offset > len(trimmed) {
		return key{}, fmt.Errorf("%s:%d:%d: column out of range", filePath, line, column)
	}
	return key{path: filePath, kind: kind, offset: offset, text: trimmed}, nil
}

// readAllow reads the list of accepted mutants.
func readAllow(allowPath string) ([]entry, error) {
	file, err := os.Open(allowPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var entries []entry
	seen := map[key]int{}
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || strings.TrimSpace(fields[4]) == "" {
			return nil, fmt.Errorf("%s:%d: want path, type, offset, line text and reason separated by tabs", allowPath, lineNo)
		}
		offset, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: offset: %w", allowPath, lineNo, err)
		}
		k := key{path: fields[0], kind: fields[1], offset: offset, text: fields[3]}
		if first, dup := seen[k]; dup {
			return nil, fmt.Errorf("%s:%d: the same mutant as line %d", allowPath, lineNo, first)
		}
		seen[k] = lineNo
		entries = append(entries, entry{key: k, lineNo: lineNo})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// inRun tells whether a path lies in one of the directories of the run.
func inRun(filePath string, dirs []string) bool {
	for _, dir := range dirs {
		if dir == "." || strings.HasPrefix(filePath, dir+"/") {
			return true
		}
	}
	return false
}

// hasStatus tells whether any of statuses is one of wanted.
func hasStatus(statuses []string, wanted ...string) bool {
	for _, status := range statuses {
		for _, w := range wanted {
			if status == w {
				return true
			}
		}
	}
	return false
}
