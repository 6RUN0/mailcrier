package spool

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// ReservePercent is the share of the total limits kept for root and the
// service user: when every other user has filled their own quota, the
// mail of root still gets queued.
const ReservePercent = 20

// Limits bound the spool; zero leaves a bound out.
type Limits struct {
	// Messages and Bytes bound all entries together.
	Messages int
	Bytes    int64
	// MessagesPerUID and BytesPerUID bound the entries of one owner.
	MessagesPerUID int
	BytesPerUID    int64
}

// Quota is what a new entry must fit in.
type Quota struct {
	Limits
	// IsReserved lets the entry use the reserve: set for root and the
	// service user.
	IsReserved bool
}

// Usage is what the entries in tmp/, queue/ and hold/ take up; failed/
// and locks/ do not count.
type Usage struct {
	Messages int
	Bytes    int64
	// PerUID is the usage of each owner, as the sidecars say; entries
	// whose sidecar is missing or unreadable count only in the totals.
	PerUID map[int]Count
}

// Count is the usage of one owner.
type Count struct {
	Messages int
	Bytes    int64
}

// Admit returns nil when an entry of size bytes owned by uid fits in q
// next to usage, else an error naming the limit. Others than root and
// the service user get the total limits less ReservePercent.
func (q Quota) Admit(usage Usage, uid int, size int64) error {
	messages, bytes := int64(q.Messages), q.Bytes
	if !q.IsReserved {
		messages -= messages * ReservePercent / 100
		bytes -= bytes * ReservePercent / 100
	}
	own := usage.PerUID[uid]
	for _, limit := range []struct {
		name        string
		used, limit int64
		isSet       bool
	}{
		{"max_queue_messages", int64(usage.Messages) + 1, messages, q.Messages > 0},
		{"max_queue_bytes", usage.Bytes + size, bytes, q.Bytes > 0},
		{"max_queue_messages_per_uid", int64(own.Messages) + 1, int64(q.MessagesPerUID), q.MessagesPerUID > 0},
		{"max_queue_bytes_per_uid", own.Bytes + size, q.BytesPerUID, q.BytesPerUID > 0},
	} {
		if limit.isSet && limit.used > limit.limit {
			return fmt.Errorf("limit %s reached", limit.name)
		}
	}
	return nil
}

// Usage adds up the message files in tmp/, hold/ and queue/ and their
// sidecars. Entries only move forward, from tmp/ to hold/ or queue/ and
// from hold/ to queue/, so reading the areas in that order counts an entry
// that moves during the scan more than once at worst, never not at all.
func (s *Spool) Usage() (Usage, error) {
	usage := Usage{PerUID: map[int]Count{}}
	for _, area := range []string{TmpDir, HoldDir, QueueDir} {
		entries, err := readDir(s.path(area, ""))
		if err != nil {
			return Usage{}, err
		}
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), messageSuffix)
			if !ok {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return Usage{}, err
			}
			size := info.Size()
			var owner *Entry
			if data, err := readFile(s.path(area, id+entrySuffix)); err == nil {
				size += int64(len(data))
				owner, _ = Decode(data)
			}
			usage.Messages++
			usage.Bytes += size
			if owner != nil {
				count := usage.PerUID[owner.OwnerUID]
				count.Messages++
				count.Bytes += size
				usage.PerUID[owner.OwnerUID] = count
			}
		}
	}
	return usage, nil
}
