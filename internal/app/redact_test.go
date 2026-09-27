package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// secretToken is the part of a webhook URL that must never reach a log.
const secretToken = "T0KEN-0123456789abcdefghij"

// secretHost is a host label that is a secret, as with services that give
// each webhook its own subdomain.
const secretHost = "eo1a2b3c4d5e6f7g8h9i0j"

// redirectingClient trusts server and connects every request to it,
// whatever host the URL names.
func redirectingClient(server *httptest.Server) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	var dialer net.Dialer
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		},
	}}
}

// panickingTransport fails the way a buggy dependency would: with a panic
// whose value quotes the request URL.
type panickingTransport struct{}

func (panickingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	panic(fmt.Sprintf("unexpected request to %s", req.URL))
}

// echoServer answers every request with status and the request URL in the
// body, as some services do in their error pages.
func echoServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "no route for %s", r.URL)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRunKeepsTokenOutOfLogs drives every error path a delivery or a
// configuration can take with a token in the target URL, and checks that
// the token appears in neither syslog nor stderr.
func TestRunKeepsTokenOutOfLogs(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the configuration and the HTTP client to use.
		setup    func(t *testing.T) (string, *http.Client)
		wantCode int
		wantLog  string
	}{
		{"connection-refused", func(*testing.T) (string, *http.Client) {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return httpTargetConfig(server.URL + "/hook/" + secretToken), nil
		}, 69, "target failed"},
		{"timeout", func(t *testing.T) (string, *http.Client) {
			return "[general]\nhttp_timeout = \"50ms\"\n\n" + httpTargetConfig(hangingServer(t).URL+"/hook/"+secretToken), nil
		}, 69, "target failed"},
		{"tls-error", func(t *testing.T) (string, *http.Client) {
			server := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(server.Close)
			return httpTargetConfig(server.URL + "/hook/" + secretToken), nil
		}, 69, "target failed"},
		{"dns-error", func(*testing.T) (string, *http.Client) {
			return "[general]\nhttp_timeout = \"5s\"\n\n" + httpTargetConfig("https://"+secretHost+".invalid/hook/"+secretToken), nil
		}, 69, "target failed"},
		{"tls-name-mismatch", func(t *testing.T) (string, *http.Client) {
			server := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(server.Close)
			return httpTargetConfig("https://" + secretHost + ".example.test/hook/" + secretToken), redirectingClient(server)
		}, 69, "target failed"},
		{"status-4xx", func(t *testing.T) (string, *http.Client) {
			return httpTargetConfig(echoServer(t, http.StatusNotFound).URL + "/hook/" + secretToken), nil
		}, 69, "target failed"},
		{"status-5xx", func(t *testing.T) (string, *http.Client) {
			return httpTargetConfig(echoServer(t, http.StatusBadGateway).URL + "/hook/" + secretToken), nil
		}, 69, "target failed"},
		{"telegram-rejected", func(t *testing.T) (string, *http.Client) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":400,"description":"Bad Request: no route for %s"}`, r.URL)
			}))
			t.Cleanup(server.Close)
			return "[target.tg]\ntype = \"telegram\"\ntoken = \"" + secretToken + "\"\nchat_id = \"1\"\n", redirectingClient(server)
		}, 69, "target failed"},
		{"slack-error-quotes-token", func(t *testing.T) (string, *http.Client) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"ok":false,"error":"invalid_auth for %s"}`, r.Header.Get("Authorization"))
			}))
			t.Cleanup(server.Close)
			return slackConfig(secretToken), rewritingClient(server)
		}, 69, "invalid_auth for Bearer ***"},
		{"slack-connection-refused", func(*testing.T) (string, *http.Client) {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return slackConfig(secretToken), rewritingClient(server)
		}, 69, "target failed"},
		{"toml-error-after-token", func(*testing.T) (string, *http.Client) {
			return "[target.tg]\ntype = \"telegram\"\ntoken = \"" + secretToken + "\"\nchat_id = \n", nil
		}, 78, "configuration rejected"},
		{"toml-unterminated-token", func(*testing.T) (string, *http.Client) {
			return "[target.tg]\ntype = \"telegram\"\ntoken = \"" + secretToken + "\nchat_id = \"1\"\n", nil
		}, 78, "configuration rejected"},
		{"toml-bare-token", func(*testing.T) (string, *http.Client) {
			return "[target.tg]\ntype = \"telegram\"\ntoken = " + secretToken + "\n", nil
		}, 78, "configuration rejected"},
		{"toml-token-in-url-key", func(*testing.T) (string, *http.Client) {
			return "[target.hook]\ntype = \"http\"\npreset = \"generic-json\"\nurl = \"https://example.org/hook/" + secretToken + "\"\n[target.hook]\n", nil
		}, 78, "configuration rejected"},
		{"panic-in-target", func(*testing.T) (string, *http.Client) {
			return httpTargetConfig("https://hooks.example.org/hook/" + secretToken), &http.Client{Transport: panickingTransport{}}
		}, 69, "panic: unexpected request"},
	}
	for _, tc := range cases {
		t.Run("T-TPL-12/"+tc.name, func(t *testing.T) {
			config, client := tc.setup(t)
			inv := &invocation{config: config, client: client, stdin: strings.NewReader("Subject: t\n\nb\n")}
			code := inv.run(t)
			if code != tc.wantCode {
				t.Errorf("Run() = %d, want %d", code, tc.wantCode)
			}
			output := inv.output()
			if !strings.Contains(output, tc.wantLog) {
				t.Errorf("output lacks %q:\n%s", tc.wantLog, output)
			}
			for _, secret := range []string{secretToken, secretHost} {
				if strings.Contains(output, secret) {
					t.Errorf("output contains %q:\n%s", secret, output)
				}
			}
		})
	}
}

// slackConfig configures one slack target with token.
func slackConfig(token string) string {
	return "[target.sl]\ntype = \"slack\"\ntoken = \"" + token + "\"\nchannel = \"#ops\"\n"
}

// TestRunRedactsStandardLog covers the writer Run installs for the log
// package, where net/http prints its HTTP/2 debug output, including the
// request path.
func TestRunRedactsStandardLog(t *testing.T) {
	server := echoServer(t, http.StatusOK)
	inv := &invocation{config: httpTargetConfig(server.URL + "/hook/" + secretToken), stdin: strings.NewReader("Subject: t\n\nb\n")}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0", code)
	}
	if inv.logOutput == nil {
		t.Fatal("Run did not redirect the log package")
	}
	_, _ = fmt.Fprintf(inv.logOutput, "http2: Transport encoding header %q = %q\n", ":path", "/hook/"+secretToken)
	if got := inv.stderr.String(); strings.Contains(got, secretToken) || !strings.Contains(got, `":path" = "***"`) {
		t.Errorf("stderr = %q, want the path masked", got)
	}
}
