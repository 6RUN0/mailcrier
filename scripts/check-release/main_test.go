package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSHA      = "0123456789abcdef0123456789abcdef01234567"
	testWorkflow = ".github/workflows/ci.yml"
	testBranch   = "develop"
)

var testJobs = []string{"make check", "make snapshot"}

// fakeAPI serves the two Actions endpoints check-release reads, pageSize
// entries per page with Link headers, and refuses a request without the
// expected token.
type fakeAPI struct {
	runs     []map[string]any
	jobs     map[int64][]job
	pageSize int
	status   int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.status != 0 {
		http.Error(w, `{"message":"Bad credentials"}`, f.status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	var items []any
	key := ""
	switch {
	case r.URL.Path == "/repos/o/r/actions/runs":
		q := r.URL.Query()
		if q.Get("head_sha") != testSHA || q.Get("event") != "push" || q.Get("branch") != testBranch {
			http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		key = "workflow_runs"
		for _, run := range f.runs {
			items = append(items, run)
		}
	case strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/runs/") && strings.HasSuffix(r.URL.Path, "/jobs"):
		if r.URL.Query().Get("filter") != "latest" {
			http.Error(w, "jobs without filter=latest", http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/runs/"), "/jobs"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		key = "jobs"
		for _, j := range f.jobs[id] {
			items = append(items, j)
		}
	default:
		http.NotFound(w, r)
		return
	}
	size := f.pageSize
	if size == 0 {
		size = 100
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	if end < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	if err := json.NewEncoder(w).Encode(map[string]any{key: items[start:end]}); err != nil {
		panic(err)
	}
}

func run(id int64, path, event, status string, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "path": path, "event": event, "head_branch": testBranch, "head_sha": testSHA,
		"status": status, "created_at": created.Format(time.RFC3339), "html_url": fmt.Sprintf("https://example.org/runs/%d", id),
	}
}

func succeeded(names ...string) []job {
	var jobs []job
	for _, name := range names {
		jobs = append(jobs, job{Name: name, Status: "completed", Conclusion: "success"})
	}
	return jobs
}

func runCheck(t *testing.T, api *fakeAPI) ([]string, error) {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	c := checker{client: server.Client(), api: server.URL, repo: "o/r", token: "secret"}
	return c.check(context.Background(), testSHA, testWorkflow, testBranch, testJobs)
}

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func TestCheckPasses(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, testWorkflow, "push", "completed", t0)},
		jobs: map[int64][]job{1: append(succeeded(testJobs...), job{Name: "make vuln", Status: "completed", Conclusion: "skipped"})},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 0 {
		t.Fatalf("check() = %q, %v, want no problems", problems, err)
	}
}

func TestCheckRejectsOtherWorkflow(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, ".github/workflows/codeql.yml", "push", "completed", t0)},
		jobs: map[int64][]job{1: succeeded(testJobs...)},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "no push run") {
		t.Fatalf("check() = %q, %v, want no push run", problems, err)
	}
}

func TestCheckRejectsScheduleRun(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, testWorkflow, "schedule", "completed", t0)},
		jobs: map[int64][]job{1: succeeded(testJobs...)},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "no push run") {
		t.Fatalf("check() = %q, %v, want no push run", problems, err)
	}
}

// The jobs endpoint with filter=latest answers for the last attempt, so an
// attempt that failed after an earlier success blocks the release.
func TestCheckRejectsFailedLatestAttempt(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, testWorkflow, "push", "completed", t0)},
		jobs: map[int64][]job{1: {
			{Name: "make check", Status: "completed", Conclusion: "failure"},
			{Name: "make snapshot", Status: "completed", Conclusion: "success"},
		}},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], `job "make check" concluded "failure"`) {
		t.Fatalf("check() = %q, %v, want make check failed", problems, err)
	}
}

func TestCheckTakesNewestRun(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{
			run(2, testWorkflow, "push", "completed", t0.Add(time.Hour)),
			run(1, testWorkflow, "push", "completed", t0),
		},
		jobs: map[int64][]job{1: succeeded(testJobs...), 2: {
			{Name: "make check", Status: "completed", Conclusion: "cancelled"},
			{Name: "make snapshot", Status: "completed", Conclusion: "success"},
		}},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "runs/2") {
		t.Fatalf("check() = %q, %v, want a problem of run 2", problems, err)
	}
}

func TestCheckRejectsRunInProgress(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, testWorkflow, "push", "in_progress", t0)},
		jobs: map[int64][]job{1: succeeded(testJobs...)},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "is in_progress") {
		t.Fatalf("check() = %q, %v, want in_progress", problems, err)
	}
}

func TestCheckRejectsMissingJob(t *testing.T) {
	api := &fakeAPI{
		runs: []map[string]any{run(1, testWorkflow, "push", "completed", t0)},
		jobs: map[int64][]job{1: succeeded("make check")},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], `no job "make snapshot"`) {
		t.Fatalf("check() = %q, %v, want missing make snapshot", problems, err)
	}
}

// The newest run sits on the second page of runs and its required jobs on
// the second page of jobs; reading only the first pages would miss both.
func TestCheckFollowsPages(t *testing.T) {
	api := &fakeAPI{
		pageSize: 1,
		runs: []map[string]any{
			run(1, testWorkflow, "push", "completed", t0),
			run(2, testWorkflow, "push", "completed", t0.Add(time.Hour)),
		},
		jobs: map[int64][]job{
			1: {{Name: "make check", Status: "completed", Conclusion: "failure"}},
			2: append([]job{{Name: "make vuln", Status: "completed", Conclusion: "skipped"}}, succeeded(testJobs...)...),
		},
	}
	problems, err := runCheck(t, api)
	if err != nil || len(problems) != 0 {
		t.Fatalf("check() = %q, %v, want no problems", problems, err)
	}
	api.jobs[2] = append(succeeded("make vuln", "make check"), job{Name: "make snapshot", Status: "completed", Conclusion: "failure"})
	problems, err = runCheck(t, api)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], `"make snapshot"`) {
		t.Fatalf("check() = %q, %v, want make snapshot failed on the last page", problems, err)
	}
}

func TestCheckFailsOnUnauthorized(t *testing.T) {
	problems, err := runCheck(t, &fakeAPI{status: http.StatusUnauthorized})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("check() = %q, %v, want a 401 error", problems, err)
	}
}

func TestCheckRefusesForeignNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://example.org/steal>; rel="next"`)
		if _, err := w.Write([]byte(`{"workflow_runs":[]}`)); err != nil {
			panic(err)
		}
	}))
	t.Cleanup(server.Close)
	c := checker{client: server.Client(), api: server.URL, repo: "o/r", token: "secret"}
	if _, err := c.check(context.Background(), testSHA, testWorkflow, testBranch, testJobs); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("check() error = %v, want a refused next page", err)
	}
}
