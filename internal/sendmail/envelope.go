package sendmail

import "github.com/6RUN0/slendmail/internal/message"

// Envelope returns the sender and recipients of msg. The sender is the
// value of -f or -r, else the From address, else what defaultSender
// returns. The recipients are those of the command line and, with -t, the
// To, Cc and bcc addresses, in that order and without duplicates.
func (inv Invocation) Envelope(msg *message.Message, bcc []message.Address, defaultSender func() string) message.Envelope {
	env := message.Envelope{SenderName: inv.FullName}
	switch {
	case inv.HasSender:
		env.Sender = inv.Sender
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
		for _, list := range [][]message.Address{msg.To, msg.Cc, bcc} {
			for _, a := range list {
				add(a.Addr)
			}
		}
	}
	return env
}
