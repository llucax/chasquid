// Package queue implements our email queue.
// Accepted envelopes get put in the queue, and processed asynchronously.
package queue

// Command to generate queue.pb.go from queue.proto.
//go:generate protoc --go_out=. -I=${GOPATH}/src -I. queue.proto

import (
	"context"
	"encoding/base64"
	"expvar"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bytes"

	"blitiri.com.ar/go/chasquid/internal/aliases"
	"blitiri.com.ar/go/chasquid/internal/courier"
	"blitiri.com.ar/go/chasquid/internal/envelope"
	"blitiri.com.ar/go/chasquid/internal/log"
	"blitiri.com.ar/go/chasquid/internal/maillog"
	"blitiri.com.ar/go/chasquid/internal/protoio"
	"blitiri.com.ar/go/chasquid/internal/set"
	"blitiri.com.ar/go/chasquid/internal/trace"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"golang.org/x/net/idna"
)

const (
	// Maximum size of the queue; we reject emails when we hit this.
	maxQueueSize = 200

	// Give up sending attempts after this duration.
	giveUpAfter = 12 * time.Hour

	// Prefix for item file names.
	// This is for convenience, versioning, and to be able to tell them apart
	// temporary files and other cruft.
	// It's important that it's outside the base64 space so it doesn't get
	// generated accidentally.
	itemFilePrefix = "m:"
)

var (
	errQueueFull = fmt.Errorf("Queue size too big, try again later")
)

// Exported variables.
var (
	putCount        = expvar.NewInt("chasquid/queue/putCount")
	itemsWritten    = expvar.NewInt("chasquid/queue/itemsWritten")
	dsnQueued       = expvar.NewInt("chasquid/queue/dsnQueued")
	deliverAttempts = expvar.NewMap("chasquid/queue/deliverAttempts")
)

// Channel used to get random IDs for items in the queue.
var newID chan string

func generateNewIDs() {
	// The IDs are only used internally, we are ok with using a PRNG.
	// We create our own to avoid relying on external sources initializing it
	// properly.
	prng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// IDs are base64(8 random bytes), but the code doesn't care.
	buf := make([]byte, 8)
	id := ""
	for {
		prng.Read(buf)
		id = base64.RawURLEncoding.EncodeToString(buf)
		newID <- id
	}
}

func init() {
	newID = make(chan string, 4)
	go generateNewIDs()
}

// Queue that keeps mail waiting for delivery.
type Queue struct {
	// Items in the queue. Map of id -> Item.
	q map[string]*Item

	// Mutex protecting q.
	mu sync.RWMutex

	// Couriers to use to deliver mail.
	localC  courier.Courier
	remoteC courier.Courier

	// Domains we consider local.
	localDomains *set.String

	// Path where we store the queue.
	path string

	// Aliases resolver.
	aliases *aliases.Resolver

	// Domain we use to send delivery status notifications from.
	dsnDomain string
}

// New creates a new Queue instance.
func New(path string, localDomains *set.String, aliases *aliases.Resolver,
	localC, remoteC courier.Courier, dsnDomain string) *Queue {

	os.MkdirAll(path, 0700)

	return &Queue{
		q:            map[string]*Item{},
		localC:       localC,
		remoteC:      remoteC,
		localDomains: localDomains,
		path:         path,
		aliases:      aliases,
		dsnDomain:    dsnDomain,
	}
}

// Load the queue and launch the sending loops on startup.
func (q *Queue) Load() error {
	files, err := filepath.Glob(q.path + "/" + itemFilePrefix + "*")
	if err != nil {
		return err
	}

	for _, fname := range files {
		item, err := ItemFromFile(fname)
		if err != nil {
			log.Errorf("error loading queue item from %q: %v", fname, err)
			continue
		}

		q.mu.Lock()
		q.q[item.ID] = item
		q.mu.Unlock()

		go item.SendLoop(q)
	}

	return nil
}

func (q *Queue) Len() int {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return len(q.q)
}

// Put an envelope in the queue.
func (q *Queue) Put(from string, to []string, data []byte) (string, error) {
	if q.Len() >= maxQueueSize {
		return "", errQueueFull
	}
	putCount.Add(1)

	item := &Item{
		Message: Message{
			ID:   <-newID,
			From: from,
			Data: data,
		},
		CreatedAt: time.Now(),
	}

	for _, t := range to {
		item.To = append(item.To, t)

		rcpts, err := q.aliases.Resolve(t)
		if err != nil {
			return "", fmt.Errorf("error resolving aliases for %q: %v", t, err)
		}

		// Add the recipients (after resolving aliases); this conversion is
		// not very pretty but at least it's self contained.
		for _, aliasRcpt := range rcpts {
			r := &Recipient{
				Address:         aliasRcpt.Addr,
				Status:          Recipient_PENDING,
				OriginalAddress: t,
			}
			switch aliasRcpt.Type {
			case aliases.EMAIL:
				r.Type = Recipient_EMAIL
			case aliases.PIPE:
				r.Type = Recipient_PIPE
			default:
				log.Errorf("unknown alias type %v when resolving %q",
					aliasRcpt.Type, t)
				return "", fmt.Errorf("internal error - unknown alias type")
			}
			item.Rcpt = append(item.Rcpt, r)
		}
	}

	err := item.WriteTo(q.path)
	if err != nil {
		return "", fmt.Errorf("failed to write item: %v", err)
	}

	q.mu.Lock()
	q.q[item.ID] = item
	q.mu.Unlock()

	// Begin to send it right away.
	go item.SendLoop(q)

	return item.ID, nil
}

// Remove an item from the queue.
func (q *Queue) Remove(id string) {
	path := fmt.Sprintf("%s/%s%s", q.path, itemFilePrefix, id)
	err := os.Remove(path)
	if err != nil {
		log.Errorf("failed to remove queue file %q: %v", path, err)
	}

	q.mu.Lock()
	delete(q.q, id)
	q.mu.Unlock()
}

// DumpString returns a human-readable string with the current queue.
// Useful for debugging purposes.
func (q *Queue) DumpString() string {
	q.mu.RLock()
	defer q.mu.RUnlock()
	s := fmt.Sprintf("# Queue status\n\n")
	s += fmt.Sprintf("date: %v\n", time.Now())
	s += fmt.Sprintf("length: %d\n\n", len(q.q))

	for id, item := range q.q {
		s += fmt.Sprintf("## Item %s\n", id)
		item.Lock()
		s += fmt.Sprintf("created at: %s\n", item.CreatedAt)
		s += fmt.Sprintf("from: %s\n", item.From)
		s += fmt.Sprintf("to: %s\n", item.To)
		for _, rcpt := range item.Rcpt {
			s += fmt.Sprintf("%s %s (%s)\n", rcpt.Status, rcpt.Address, rcpt.Type)
			s += fmt.Sprintf("  original address: %s\n", rcpt.OriginalAddress)
			s += fmt.Sprintf("  last failure: %q\n", rcpt.LastFailureMessage)
		}
		item.Unlock()
		s += fmt.Sprintf("\n")
	}

	return s
}

// An item in the queue.
type Item struct {
	// Base the item on the protobuf message.
	// We will use this for serialization, so any fields below are NOT
	// serialized.
	Message

	// Protect the entire item.
	sync.Mutex

	// Go-friendly version of Message.CreatedAtTs.
	CreatedAt time.Time
}

func ItemFromFile(fname string) (*Item, error) {
	item := &Item{}
	err := protoio.ReadTextMessage(fname, &item.Message)
	if err != nil {
		return nil, err
	}

	item.CreatedAt, err = ptypes.Timestamp(item.CreatedAtTs)
	return item, err
}

func (item *Item) WriteTo(dir string) error {
	item.Lock()
	defer item.Unlock()
	itemsWritten.Add(1)

	var err error
	item.CreatedAtTs, err = ptypes.TimestampProto(item.CreatedAt)
	if err != nil {
		return err
	}

	path := fmt.Sprintf("%s/%s%s", dir, itemFilePrefix, item.ID)

	return protoio.WriteTextMessage(path, &item.Message, 0600)
}

func (item *Item) SendLoop(q *Queue) {
	tr := trace.New("Queue.SendLoop", item.ID)
	defer tr.Finish()
	tr.Printf("from %s", item.From)

	for time.Since(item.CreatedAt) < giveUpAfter {
		// Send to all recipients that are still pending.
		var wg sync.WaitGroup
		for _, rcpt := range item.Rcpt {
			if rcpt.Status != Recipient_PENDING {
				continue
			}

			wg.Add(1)
			go item.sendOneRcpt(&wg, tr, q, rcpt)
		}
		wg.Wait()

		// If they're all done, no need to wait.
		if item.countRcpt(Recipient_PENDING) == 0 {
			break
		}

		// TODO: Consider sending a non-final notification after 30m or so,
		// that some of the messages have been delayed.

		delay := nextDelay(item.CreatedAt)
		tr.Printf("waiting for %v", delay)
		maillog.QueueLoop(item.ID, item.From, delay)
		time.Sleep(delay)
	}

	// Completed to all recipients (some may not have succeeded).
	if item.countRcpt(Recipient_FAILED, Recipient_PENDING) > 0 && item.From != "<>" {
		sendDSN(tr, q, item)
	}

	tr.Printf("all done")
	maillog.QueueLoop(item.ID, item.From, 0)
	q.Remove(item.ID)
}

// sendOneRcpt, and update it with the results.
func (item *Item) sendOneRcpt(wg *sync.WaitGroup, tr *trace.Trace, q *Queue, rcpt *Recipient) {
	defer wg.Done()
	to := rcpt.Address
	tr.Debugf("%s sending", to)

	err, permanent := item.deliver(q, rcpt)

	item.Lock()
	if err != nil {
		rcpt.LastFailureMessage = err.Error()
		if permanent {
			tr.Errorf("%s permanent error: %v", to, err)
			maillog.SendAttempt(item.ID, item.From, to, err, true)
			rcpt.Status = Recipient_FAILED
		} else {
			tr.Printf("%s temporary error: %v", to, err)
			maillog.SendAttempt(item.ID, item.From, to, err, false)
		}
	} else {
		tr.Printf("%s sent", to)
		maillog.SendAttempt(item.ID, item.From, to, nil, false)
		rcpt.Status = Recipient_SENT
	}
	item.Unlock()

	err = item.WriteTo(q.path)
	if err != nil {
		tr.Errorf("failed to write: %v", err)
	}
}

// deliver the item to the given recipient, using the couriers from the queue.
// Return an error (if any), and whether it is permanent or not.
func (item *Item) deliver(q *Queue, rcpt *Recipient) (err error, permanent bool) {
	if rcpt.Type == Recipient_PIPE {
		deliverAttempts.Add("pipe", 1)
		c := strings.Fields(rcpt.Address)
		if len(c) == 0 {
			return fmt.Errorf("empty pipe"), true
		}
		ctx, cancel := context.WithDeadline(context.Background(),
			time.Now().Add(30*time.Second))
		defer cancel()
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		cmd.Stdin = bytes.NewReader(item.Data)
		return cmd.Run(), true
	}

	// Recipient type is EMAIL.
	if envelope.DomainIn(rcpt.Address, q.localDomains) {
		deliverAttempts.Add("email:local", 1)
		return q.localC.Deliver(item.From, rcpt.Address, item.Data)
	} else {
		deliverAttempts.Add("email:remote", 1)
		from := item.From
		if !envelope.DomainIn(item.From, q.localDomains) {
			// We're sending from a non-local to a non-local. This should
			// happen only when there's an alias to forward email to a
			// non-local domain.  In this case, using the original From is
			// problematic, as we may not be an authorized sender for this.
			// Some MTAs (like Exim) will do it anyway, others (like
			// gmail) will construct a special address based on the
			// original address.  We go with the latter.
			// Note this assumes "+" is an alias suffix separator.
			// We use the IDNA version of the domain if possible, because
			// we can't know if the other side will support SMTPUTF8.
			from = fmt.Sprintf("%s+fwd_from=%s@%s",
				envelope.UserOf(rcpt.OriginalAddress),
				strings.Replace(from, "@", "=", -1),
				mustIDNAToASCII(envelope.DomainOf(rcpt.OriginalAddress)))
		}
		return q.remoteC.Deliver(from, rcpt.Address, item.Data)
	}
}

// countRcpt counts how many recipients are in the given status.
func (item *Item) countRcpt(statuses ...Recipient_Status) int {
	c := 0
	for _, rcpt := range item.Rcpt {
		for _, status := range statuses {
			if rcpt.Status == status {
				c++
				break
			}
		}
	}
	return c
}

func sendDSN(tr *trace.Trace, q *Queue, item *Item) {
	tr.Debugf("sending DSN")

	msg, err := deliveryStatusNotification(q.dsnDomain, item)
	if err != nil {
		tr.Errorf("failed to build DSN: %v", err)
		return
	}

	id, err := q.Put("<>", []string{item.From}, msg)
	if err != nil {
		tr.Errorf("failed to queue DSN: %v", err)
		return
	}

	tr.Printf("queued DSN: %s", id)
	dsnQueued.Add(1)
}

func nextDelay(createdAt time.Time) time.Duration {
	var delay time.Duration

	since := time.Since(createdAt)
	switch {
	case since < 1*time.Minute:
		delay = 1 * time.Minute
	case since < 5*time.Minute:
		delay = 5 * time.Minute
	case since < 10*time.Minute:
		delay = 10 * time.Minute
	default:
		delay = 20 * time.Minute
	}

	// Perturb the delay, to avoid all queued emails to be retried at the
	// exact same time after a restart.
	delay += time.Duration(rand.Intn(60)) * time.Second
	return delay
}

func timestampNow() *timestamp.Timestamp {
	now := time.Now()
	ts, _ := ptypes.TimestampProto(now)
	return ts
}

func mustIDNAToASCII(s string) string {
	a, err := idna.ToASCII(s)
	if err != nil {
		return a
	}
	return s
}
