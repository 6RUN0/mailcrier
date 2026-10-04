// Command check-release fails unless the CI of a commit passed: a release is
// built from a tag, and the tag must point at a commit whose push run of the
// CI workflow on the development branch finished with every required job
// successful.
//
// Usage:
//
//	go run ./scripts/check-release -sha SHA -workflow .github/workflows/ci.yml \
//		-branch develop -jobs "make check,make snapshot"
//
// It reads the GitHub Actions API at GITHUB_API_URL (default
// https://api.github.com) for the repository GITHUB_REPOSITORY (owner/name)
// with the token in GH_TOKEN. Of the push runs of the workflow for the commit
// on the branch the newest by creation time counts; jobs are those of its
// latest attempt. Findings go to stdout, one per line; the exit status is 1
// when there are findings and 2 when the check itself fails.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// requestTimeout bounds the whole check; the Actions API answers a page in
// well under a second, so a minute only cuts a hang short.
const requestTimeout = time.Minute

func main() {
	sha := flag.String("sha", "", "commit whose CI run is checked")
	workflow := flag.String("workflow", "", "path of the workflow file, as .github/workflows/ci.yml")
	branch := flag.String("branch", "", "branch of the push run")
	jobs := flag.String("jobs", "", "comma-separated names of the jobs that must succeed")
	flag.Parse()
	if *sha == "" || *workflow == "" || *branch == "" || *jobs == "" {
		flag.Usage()
		os.Exit(2)
	}
	repo := os.Getenv("GITHUB_REPOSITORY")
	token := os.Getenv("GH_TOKEN")
	api := os.Getenv("GITHUB_API_URL")
	if api == "" {
		api = "https://api.github.com"
	}
	if repo == "" || token == "" {
		fmt.Fprintln(os.Stderr, "check-release: GITHUB_REPOSITORY and GH_TOKEN must be set")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	c := checker{client: http.DefaultClient, api: strings.TrimSuffix(api, "/"), repo: repo, token: token}
	problems, err := c.check(ctx, *sha, *workflow, *branch, strings.Split(*jobs, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-release:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}

type checker struct {
	client *http.Client
	api    string
	repo   string
	token  string
}

type workflowRun struct {
	ID         int64     `json:"id"`
	Path       string    `json:"path"`
	Event      string    `json:"event"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	HTMLURL    string    `json:"html_url"`
}

type job struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// check returns the reasons the commit may not be released, or an error
// when the API cannot answer.
func (c checker) check(ctx context.Context, sha, workflow, branch string, required []string) ([]string, error) {
	query := url.Values{
		"head_sha": {sha},
		"event":    {"push"},
		"branch":   {branch},
		"per_page": {"100"},
	}
	var latest *workflowRun
	err := c.pages(ctx, "/repos/"+c.repo+"/actions/runs?"+query.Encode(), func(body []byte) error {
		var page struct {
			Runs []workflowRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for i := range page.Runs {
			run := page.Runs[i]
			// The query already narrows the runs; the fields are checked
			// again so that a run of another event or branch never counts.
			if run.Path != workflow || run.Event != "push" || run.HeadBranch != branch || run.HeadSHA != sha {
				continue
			}
			if latest == nil || run.CreatedAt.After(latest.CreatedAt) {
				latest = &run
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return []string{fmt.Sprintf("no push run of %s on %s for %s: CI runs only for the head of a push, so tag a commit that has its own push run", workflow, branch, sha)}, nil
	}
	if latest.Status != "completed" {
		return []string{fmt.Sprintf("run %s is %s, not completed", latest.HTMLURL, latest.Status)}, nil
	}
	conclusions := map[string][]string{}
	err = c.pages(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", c.repo, latest.ID), func(body []byte) error {
		var page struct {
			Jobs []job `json:"jobs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, j := range page.Jobs {
			conclusions[j.Name] = append(conclusions[j.Name], j.Conclusion)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, name := range required {
		found, ok := conclusions[name]
		if !ok {
			problems = append(problems, fmt.Sprintf("run %s has no job %q", latest.HTMLURL, name))
			continue
		}
		for _, conclusion := range found {
			if conclusion != "success" {
				problems = append(problems, fmt.Sprintf("run %s: job %q concluded %q, not success", latest.HTMLURL, name, conclusion))
			}
		}
	}
	return problems, nil
}

// nextLink finds the URL of the next page in a Link header.
var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pages fetches path and every page its Link headers name after it, handing
// each body to handle.
func (c checker) pages(ctx context.Context, path string, handle func([]byte) error) error {
	next := c.api + path
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s: %s", req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
		}
		if err := handle(body); err != nil {
			return fmt.Errorf("GET %s: %w", req.URL.Path, err)
		}
		next = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			// The token goes with every request, so a page elsewhere is refused.
			if !strings.HasPrefix(m[1], c.api+"/") {
				return fmt.Errorf("GET %s: next page %s is outside %s", req.URL.Path, m[1], c.api)
			}
			next = m[1]
		}
	}
	return nil
}
