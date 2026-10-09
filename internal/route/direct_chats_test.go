package route

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/6RUN0/mailcrier/internal/config"
)

// loadDirectChat loads a configuration whose telegram_direct_chats holds
// the one TOML value element and returns the chat config.Load made of it.
func loadDirectChat(t *testing.T, element string) (chat string, ok bool) {
	t.Helper()
	doc := "[general]\ntelegram_direct = \"tg\"\ntelegram_direct_chats = [" + element + "]\n\n" +
		"[target.tg]\ntype = \"telegram\"\ntoken = \"1:a\"\nchat_id = 1\n"
	cfg, err := config.Load(fstest.MapFS{"etc/mailcrier.conf": {Data: []byte(doc)}}, "etc/mailcrier.conf")
	if err != nil {
		return "", false
	}
	if len(cfg.General.TelegramDirectChats) != 1 {
		t.Fatalf("TelegramDirectChats = %q, want one chat", cfg.General.TelegramDirectChats)
	}
	return cfg.General.TelegramDirectChats[0], true
}

// TestDirectChatsMatchNormalizeChat pins that the allowed list of
// config.Load and the recipient side of SplitDirect agree on a chat: the
// two packages keep their own username expression, because config imports
// no package of the module. A string is allowed only as an @username,
// where NormalizeChat takes digits too: the list holds one spelling of a
// numeric chat, the TOML integer.
func TestDirectChatsMatchNormalizeChat(t *testing.T) {
	for _, value := range []string{
		"@abcd", "@abcde", "@Ops_Channel", "@OPS_CHANNEL", "@_____", "@a_b_c_d",
		"@" + strings.Repeat("a", 32), "@" + strings.Repeat("a", 33),
		"@abc-de", "@abc.de", "@abcdé", "@абвгде",
		"abcdef", "@@abcde", "@ abcde", " @abcde", "@abcde ", "@", "",
		"1234", "-100123", "+1", "0123",
	} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			got, isAllowed := loadDirectChat(t, strconv.Quote(value))
			want, isChat := NormalizeChat(value)
			isUsername := isChat && strings.HasPrefix(value, "@")
			if isAllowed != isUsername || got != want && isAllowed {
				t.Errorf("config.Load = %q, %v; NormalizeChat = %q, %v", got, isAllowed, want, isChat)
			}
		})
	}
	for _, value := range []int64{0, 1234, -100123, math.MaxInt64, math.MinInt64} {
		t.Run(strconv.FormatInt(value, 10), func(t *testing.T) {
			got, isAllowed := loadDirectChat(t, strconv.FormatInt(value, 10))
			want, isChat := NormalizeChat(strconv.FormatInt(value, 10))
			if !isAllowed || !isChat || got != want {
				t.Errorf("config.Load = %q, %v; NormalizeChat = %q, %v", got, isAllowed, want, isChat)
			}
		})
	}
}
