package route

import (
	"regexp"
	"slices"
	"testing"
)

// TestTargets pins the order of the rules, continue, the join over
// recipients and the message without recipients.
func TestTargets(t *testing.T) {
	raid := regexp.MustCompile(`(?is)^.*raid.*$`)
	backup := regexp.MustCompile(`^backup(@|$)`)
	root := regexp.MustCompile(`^root(@|$)`)
	router := New([]Rule{
		{Subject: raid, Targets: []string{"ops"}, IsContinued: true},
		{Recipient: backup, Targets: []string{"backup"}},
		{Recipient: root, Targets: []string{"mm"}},
	}, nil)
	cases := []struct {
		name, subject string
		recipients    []string
		want          []string
		unrouted      int
	}{
		{"continue-adds-the-next-match", "RAID degraded", []string{"backup@example.org"}, []string{"backup", "ops"}, 0},
		{"first-match-stops", "disk", []string{"root"}, []string{"mm"}, 0},
		{"each-recipient-routed-apart", "disk", []string{"root", "backup"}, []string{"backup", "mm"}, 0},
		{"unrouted-recipient-counted", "disk", []string{"root", "alice@example.org"}, []string{"mm"}, 1},
		{"no-rule-matches", "disk", []string{"alice@example.org"}, nil, 1},
		{"no-recipient-skips-recipient-rules", "disk", nil, nil, 0},
		{"no-recipient-subject-rule", "raid", nil, []string{"ops"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := router.Targets(tc.subject, "", tc.recipients)
			if got.IsImplicit || !slices.Equal(got.Names, tc.want) || got.Unrouted != tc.unrouted {
				t.Errorf("Targets() = %+v, want names %v, unrouted %d", got, tc.want, tc.unrouted)
			}
		})
	}
	t.Run("catch-all-matches-without-recipients", func(t *testing.T) {
		catchAll := New([]Rule{{Recipient: root, Targets: []string{"mm"}}, {Targets: []string{"all"}}}, nil)
		if got := catchAll.Targets("x", "", nil); !slices.Equal(got.Names, []string{"all"}) {
			t.Errorf("Targets() = %+v, want all", got)
		}
	})
	t.Run("sender-condition", func(t *testing.T) {
		bySender := New([]Rule{{Sender: regexp.MustCompile(`@backup\.example\.org$`), Targets: []string{"backup"}}, {Targets: []string{"mm"}}}, nil)
		if got := bySender.Targets("x", "cron@backup.example.org", []string{"root"}); !slices.Equal(got.Names, []string{"backup"}) {
			t.Errorf("Targets() = %+v, want backup", got)
		}
	})
	t.Run("all-conditions-must-match", func(t *testing.T) {
		both := New([]Rule{{Subject: raid, Recipient: root, Targets: []string{"ops"}}}, nil)
		if got := both.Targets("raid", "", []string{"alice"}); len(got.Names) != 0 || got.Unrouted != 1 {
			t.Errorf("Targets() = %+v, want no targets", got)
		}
	})
	t.Run("regex-without-anchors-finds-a-substring", func(t *testing.T) {
		loose := New([]Rule{{Subject: regexp.MustCompile(`err`), Targets: []string{"ops"}}}, nil)
		if got := loose.Targets("disk error", "", nil); !slices.Equal(got.Names, []string{"ops"}) {
			t.Errorf("Targets() = %+v, want ops", got)
		}
	})
	t.Run("no-rules-is-implicit", func(t *testing.T) {
		if got := New(nil, nil).Targets("x", "", []string{"root"}); !got.IsImplicit || got.Names != nil {
			t.Errorf("Targets() = %+v, want implicit", got)
		}
	})
}

// TestSuppressed pins that the first matching rule is reported, numbered
// from 1, and that every condition of a rule must match.
func TestSuppressed(t *testing.T) {
	router := New(nil, []Suppression{
		{Subject: regexp.MustCompile(`(?is)^.*anacron.*$`), Sender: regexp.MustCompile(`^root@`)},
		{Subject: regexp.MustCompile(`(?is)^.*anacron.*$`)},
	})
	if rule, ok := router.Suppressed("Anacron job", "root@host"); !ok || rule != 1 {
		t.Errorf("Suppressed() = %d, %v; want 1", rule, ok)
	}
	if rule, ok := router.Suppressed("Anacron job", "alice@host"); !ok || rule != 2 {
		t.Errorf("Suppressed() = %d, %v; want 2", rule, ok)
	}
	if _, ok := router.Suppressed("disk", "root@host"); ok {
		t.Error("Suppressed() matched a message no rule names")
	}
	if !router.HasRules() || New(nil, nil).HasRules() {
		t.Error("HasRules() wrong")
	}
}

// TestAddresses pins the normalization of recipients before routing: the
// command line passes them as written.
func TestAddresses(t *testing.T) {
	got := Addresses([]string{"Root <root@example.org>", "root", "root@example.org", "a@example.org, b@example.org", "(Cron Daemon)", ""})
	want := []string{"root@example.org", "root", "a@example.org", "b@example.org"}
	if !slices.Equal(got, want) {
		t.Errorf("Addresses() = %q, want %q", got, want)
	}
}

// TestSplitDirect pins which addresses name a direct chat.
func TestSplitDirect(t *testing.T) {
	t.Run("T-ADJ-57/numeric-id", splitCase{[]string{"1234@telegram"}, []string{"1234"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/leading-zero", splitCase{[]string{"01234@telegram"}, []string{"1234"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/negative-group-id", splitCase{[]string{"-100123@telegram"}, []string{"-100123"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/username-any-case", splitCase{[]string{"@Ops_Channel@telegram"}, []string{"@ops_channel"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/domain-any-case", splitCase{[]string{"1234@TeleGram"}, []string{"1234"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/empty-local-part", splitCase{[]string{"@telegram"}, nil, []string{"@telegram"}, 1, 0}.check)
	t.Run("T-ADJ-57/word-local-part", splitCase{[]string{"abc@telegram"}, nil, []string{"abc@telegram"}, 1, 0}.check)
	t.Run("T-ADJ-57/plus-sign", splitCase{[]string{"+1234@telegram"}, nil, []string{"+1234@telegram"}, 1, 0}.check)
	t.Run("T-ADJ-57/overflow", splitCase{[]string{"99999999999999999999@telegram"}, nil, []string{"99999999999999999999@telegram"}, 1, 0}.check)
	t.Run("T-ADJ-57/other-domain", splitCase{[]string{"1234@telegram.org"}, nil, []string{"1234@telegram.org"}, 0, 0}.check)
	t.Run("T-ADJ-57/chat-not-allowed", splitCase{[]string{"999@telegram"}, nil, []string{"999@telegram"}, 0, 1}.check)
	t.Run("T-ADJ-57/duplicates-once", splitCase{[]string{"1234@telegram", "root", "01234@telegram"}, []string{"1234"}, []string{"root"}, 0, 0}.check)
	t.Run("T-ADJ-57/addresses-from-argv", splitCase{Addresses([]string{"Ops <1234@telegram>"}), []string{"1234"}, nil, 0, 0}.check)
	t.Run("T-ADJ-57/over-the-limit", func(t *testing.T) {
		got := SplitDirect([]string{"1234@telegram", "-100123@telegram", "@ops_channel@telegram", "-100123@telegram", "@OPS_channel@telegram"}, testAllowed, 1)
		if !slices.Equal(got.Chats, []string{"1234"}) || got.Rest != nil || got.Dropped != 2 {
			t.Errorf("SplitDirect() = %+v, want chat 1234 and 2 dropped", got)
		}
	})
}

// testAllowed are the allowed chats of TestSplitDirect.
var testAllowed = []string{"1234", "-100123", "@ops_channel"}

type splitCase struct {
	recipients, chats, rest []string
	invalid, notAllowed     int
}

func (tc splitCase) check(t *testing.T) {
	t.Helper()
	got := SplitDirect(tc.recipients, testAllowed, 10)
	if !slices.Equal(got.Chats, tc.chats) || !slices.Equal(got.Rest, tc.rest) || got.Invalid != tc.invalid || got.NotAllowed != tc.notAllowed || got.Dropped != 0 {
		t.Errorf("SplitDirect() = %+v, want chats %q, rest %q, invalid %d, not allowed %d", got, tc.chats, tc.rest, tc.invalid, tc.notAllowed)
	}
}

// TestNormalizeChat pins the spellings of one chat.
func TestNormalizeChat(t *testing.T) {
	for chat, want := range map[string]string{"01234": "1234", "-0100": "-100", "@Ops_Channel": "@ops_channel", "0": "0"} {
		if got, ok := NormalizeChat(chat); !ok || got != want {
			t.Errorf("NormalizeChat(%q) = %q, %v; want %q", chat, got, ok, want)
		}
	}
	for _, chat := range []string{"", "+1", "@abc", "1 2", " 12", "@" + string(make([]byte, 33)), "1e3", "0x10"} {
		if got, ok := NormalizeChat(chat); ok {
			t.Errorf("NormalizeChat(%q) = %q, want rejected", chat, got)
		}
	}
}
