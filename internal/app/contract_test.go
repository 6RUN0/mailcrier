package app

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The contract tests run a message through Run to a real server and read it
// back from that server. make contract starts the servers in docker and
// sets these variables to their base URLs; without one its tests skip.
const (
	contractNtfyEnv       = "MAILCRIER_CONTRACT_NTFY_URL"
	contractMattermostEnv = "MAILCRIER_CONTRACT_MATTERMOST_URL"
)

// The contract message: a subject with markup characters of every target,
// a channel-wide mention and Cyrillic, a body with the same, and one text
// attachment.
const (
	contractSubject  = "Диск *full* <md0> & [raid]_1 #ops @channel"
	contractBody     = "md0 degraded\nПроверьте массив <now> & *срочно* @all"
	contractFileName = "report.txt"
	contractFile     = "строка 1\nline 2\n"
)

// contractMessage returns the contract message as cron would pipe it, the
// subject as an encoded word and the attachment in base64.
func contractMessage() string {
	return "From: cron@example.org\n" +
		"To: root@example.org\n" +
		"Subject: " + mime.BEncoding.Encode("UTF-8", contractSubject) + "\n" +
		"MIME-Version: 1.0\n" +
		"Content-Type: multipart/mixed; boundary=\"b1\"\n" +
		"\n" +
		"--b1\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"Content-Transfer-Encoding: 8bit\n" +
		"\n" +
		contractBody + "\n" +
		"--b1\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"Content-Disposition: attachment; filename=" + contractFileName + "\n" +
		"Content-Transfer-Encoding: base64\n" +
		"\n" +
		base64.StdEncoding.EncodeToString([]byte(contractFile)) + "\n" +
		"--b1--\n"
}

// contractAttachmentLine is the line the plain and ntfy templates write
// for the attachment.
var contractAttachmentLine = fmt.Sprintf("%s (text/plain, %d B)", contractFileName, len(contractFile))

// contractServer returns the base URL in the variable env, skipping the
// test without it.
func contractServer(t *testing.T, env string) string {
	t.Helper()
	base := strings.TrimSuffix(os.Getenv(env), "/")
	if base == "" {
		t.Skipf("%s is not set; make contract runs this test", env)
	}
	return base
}

// contractName returns a random lower-case name with prefix, valid as an
// ntfy topic and as a Mattermost user, team and channel name.
func contractName(prefix string) string {
	return prefix + strings.ToLower(rand.Text()[:12])
}

// runContract runs the contract message through Run with config, the spool
// off, and fails the test unless it exits 0.
func runContract(t *testing.T, config string) {
	t.Helper()
	inv := &invocation{config: config, client: &http.Client{Timeout: 30 * time.Second}, stdin: strings.NewReader(contractMessage())}
	if code := inv.run(t); code != 0 {
		t.Fatalf("Run() = %d, want 0; log:\n%s", code, inv.output())
	}
}

// contractGet returns the body of a GET of target, failing the test unless
// the status is 200.
func contractGet(t *testing.T, client *http.Client, target string, header http.Header) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", target, resp.StatusCode, body)
	}
	return body
}

// ntfyMessage is one message event of an ntfy topic feed.
type ntfyMessage struct {
	Event      string `json:"event"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	Attachment *struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size"`
		URL  string `json:"url"`
	} `json:"attachment"`
}

// pollNtfy returns the messages the server keeps for topic, oldest first.
func pollNtfy(t *testing.T, base, topic string) []ntfyMessage {
	t.Helper()
	body := contractGet(t, http.DefaultClient, base+"/"+topic+"/json?poll=1", nil)
	var messages []ntfyMessage
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		var m ntfyMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			t.Fatalf("feed line %q: %v", scanner.Text(), err)
		}
		messages = append(messages, m)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return messages
}

// TestContractNtfy delivers the contract message to an ntfy topic: the
// first message carries the subject as title and the body with the line of
// the attachment as text, the second the attachment with the subject as
// title, its name, size and content.
func TestContractNtfy(t *testing.T) {
	t.Parallel()
	base := contractServer(t, contractNtfyEnv)
	topic := contractName("mailcrier-")
	runContract(t, "[target.phone]\ntype = \"ntfy\"\nurl = \""+base+"/"+topic+"\"\n")

	messages := pollNtfy(t, base, topic)
	if len(messages) != 2 {
		t.Fatalf("topic holds %d messages, want 2: %+v", len(messages), messages)
	}
	text, file := messages[0], messages[1]
	if text.Title != contractSubject {
		t.Errorf("title = %q, want %q", text.Title, contractSubject)
	}
	if want := contractBody + "\n" + contractAttachmentLine; text.Message != want {
		t.Errorf("message = %q, want %q", text.Message, want)
	}
	if text.Attachment != nil {
		t.Errorf("text message has an attachment: %+v", *text.Attachment)
	}
	if file.Title != contractSubject {
		t.Errorf("title of the file message = %q, want %q", file.Title, contractSubject)
	}
	if file.Attachment == nil {
		t.Fatalf("second message has no attachment: %+v", file)
	}
	if file.Attachment.Name != contractFileName || file.Attachment.Size != int64(len(contractFile)) {
		t.Errorf("attachment = %q of %d bytes, want %q of %d", file.Attachment.Name, file.Attachment.Size, contractFileName, len(contractFile))
	}
	// The server names its files under the base URL of its configuration,
	// which is not the address the test reaches it at.
	link, err := url.Parse(file.Attachment.URL)
	if err != nil {
		t.Fatal(err)
	}
	if content := contractGet(t, http.DefaultClient, base+link.Path, nil); string(content) != contractFile {
		t.Errorf("attachment content = %q, want %q", content, contractFile)
	}
}

// mattermostAPI calls API v4 of a Mattermost server as one user.
type mattermostAPI struct {
	t     *testing.T
	base  string
	token string
}

// call sends in as JSON with method to path and decodes the answer into
// out, failing the test unless the status is want; it returns the header
// of the answer.
func (api *mattermostAPI) call(method, path string, in, out any, want int) http.Header {
	api.t.Helper()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			api.t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(api.t.Context(), method, api.base+"/api/v4"+path, body)
	if err != nil {
		api.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if api.token != "" {
		req.Header.Set("Authorization", "Bearer "+api.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		api.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		api.t.Fatal(err)
	}
	if resp.StatusCode != want {
		api.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			api.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
	return resp.Header
}

// mattermostPost is a post as API v4 returns it.
type mattermostPost struct {
	Message string         `json:"message"`
	Props   map[string]any `json:"props"`
	FileIDs []string       `json:"file_ids"`
}

// webhookPosts returns the posts of webhooks in a channel.
func (api *mattermostAPI) webhookPosts(channelID string) []mattermostPost {
	api.t.Helper()
	var list struct {
		Posts map[string]mattermostPost `json:"posts"`
	}
	api.call(http.MethodGet, "/channels/"+channelID+"/posts", nil, &list, http.StatusOK)
	var posts []mattermostPost
	for _, post := range list.Posts {
		if post.Props["from_webhook"] == "true" {
			posts = append(posts, post)
		}
	}
	return posts
}

// TestContractMattermost delivers the contract message through an incoming
// webhook of a Mattermost server with the mattermost preset: the post lands
// in the channel the target names, not the one of the webhook, under the
// username of the target, with subject, host, sender and body as the
// preset promises, markup escaped and channel-wide mentions disarmed, and
// without files.
func TestContractMattermost(t *testing.T) {
	t.Parallel()
	base := contractServer(t, contractMattermostEnv)
	api := &mattermostAPI{t: t, base: base}
	// The first user of a server becomes its system administrator; any
	// later one creates its own team and administers that, which is enough
	// for an incoming webhook.
	user, password := contractName("u"), "Contract-test-password-1"
	api.call(http.MethodPost, "/users", map[string]string{"email": user + "@example.org", "username": user, "password": password}, nil, http.StatusCreated)
	header := api.call(http.MethodPost, "/users/login", map[string]string{"login_id": user, "password": password}, nil, http.StatusOK)
	if api.token = header.Get("Token"); api.token == "" {
		t.Fatal("login gave no token")
	}
	var team struct {
		ID string `json:"id"`
	}
	teamName := contractName("t")
	api.call(http.MethodPost, "/teams", map[string]string{"name": teamName, "display_name": teamName, "type": "O"}, &team, http.StatusCreated)
	createChannel := func() (id, name string) {
		var channel struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		name = contractName("c")
		api.call(http.MethodPost, "/channels", map[string]string{"team_id": team.ID, "name": name, "display_name": name, "type": "O"}, &channel, http.StatusCreated)
		return channel.ID, channel.Name
	}
	hookChannel, _ := createChannel()
	targetChannel, targetName := createChannel()
	var hook struct {
		ID string `json:"id"`
	}
	api.call(http.MethodPost, "/hooks/incoming", map[string]string{"channel_id": hookChannel, "display_name": "mailcrier"}, &hook, http.StatusCreated)

	const username = "mailcrier-contract"
	runContract(t, "[target.mm]\ntype = \"http\"\npreset = \"mattermost\"\nurl = \""+base+"/hooks/"+hook.ID+"\"\n"+
		"username = \""+username+"\"\nchannel = \""+targetName+"\"\n")

	if posts := api.webhookPosts(hookChannel); len(posts) != 0 {
		t.Errorf("channel of the webhook got %d posts: %+v", len(posts), posts)
	}
	posts := api.webhookPosts(targetChannel)
	if len(posts) != 1 {
		t.Fatalf("target channel got %d webhook posts, want 1: %+v", len(posts), posts)
	}
	post := posts[0]
	want := "#### Диск \\*full\\* \\<md0\\> \\& \\[raid\\]\\_1 \\#ops @\u200bchannel\n" +
		"_host1.example.org: cron@example.org_\n" +
		"```\nmd0 degraded\nПроверьте массив <now> & *срочно* @\u200ball\n```"
	if post.Message != want {
		t.Errorf("message = %q, want %q", post.Message, want)
	}
	if got := post.Props["override_username"]; got != username {
		t.Errorf("override_username = %v, want %q", got, username)
	}
	if len(post.FileIDs) != 0 {
		t.Errorf("post has files %v, want none", post.FileIDs)
	}
}
