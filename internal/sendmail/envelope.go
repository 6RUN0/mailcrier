package sendmail

import "github.com/6RUN0/mailcrier/internal/message"

// Envelope returns the sender and recipients of msg. The sender is the
// value of -f or -r, else the Resent-From address, else the From address,
// else what defaultSender returns. The recipients are those of the command
// line and, with -t, the Resent-To, Resent-Cc and Resent-Bcc addresses
// when msg has any of these headers, else the To, Cc and Bcc addresses, in
// that order and without duplicates: a resent message goes to the
// addresses it is resent to, as with the -t of Postfix.
func (inv Invocation) Envelope(msg *message.Message, blind message.BlindCopies, defaultSender func() string) message.Envelope {
	env := message.Envelope{SenderName: inv.FullName}
	switch {
	case inv.HasSender:
		env.Sender = inv.Sender
	case msg.ResentFrom.Addr != "":
		env.Sender = msg.ResentFrom.Addr
	case msg.From.Addr != "":
		env.Sender = msg.From.Addr
	default:
		env.Sender = defaultSender()
	}
	seen := map[string]bool{}
	add := func(address string) {
		if address != "" && !seen[address] {
			seen[address] = true
			env.Recipients = append(env.Recipients, address)
		}
	}
	for _, address := range inv.Recipients {
		add(address)
	}
	if inv.ExtractRecipients {
		lists := [][]message.Address{msg.To, msg.Cc, blind.Bcc}
		if msg.IsResent {
			lists = [][]message.Address{msg.ResentTo, msg.ResentCc, blind.ResentBcc}
		}
		for _, list := range lists {
			for _, a := range list {
				add(a.Addr)
			}
		}
	}
	return env
}
