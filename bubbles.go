package bubbles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultMaxDocumentsPerBatch is the number of documents a batch needs to
	// have before it is send. This is per connection.
	DefaultMaxDocumentsPerBatch = 10

	// DefaultFlushTimeout is the maximum time we batch something before we try
	// to send it to a server.
	DefaultFlushTimeout = 10 * time.Second

	// DefaultServerTimeout is the time we give ES to respond. This is also the
	// maximum time Stop() will take.
	DefaultServerTimeout = 10 * time.Second

	// DefaultConnCount is the number of connections per hosts.
	DefaultConnCount = 2

	serverErrorWait = 1 * time.Second
)

// Bubbles is the main struct to control a queue of Actions going to the
// ElasticSearch servers.
type Bubbles struct {
	q                chan Action
	retryQ           chan Action
	error            chan ActionError
	quit             chan struct{}
	wg               sync.WaitGroup
	maxDocumentCount int
	connCount        int
	flushTimeout     time.Duration
	serverTimeout    time.Duration
}

type bulkRes struct {
	Took   int                        `json:"took"`
	Items  []map[string]bulkResStatus `json:"items"`
	Errors bool                       `json:"errors"`
}

type bulkResStatus struct {
	Index   string `json:"_index"`
	Type    string `json:"_type"`
	ID      string `json:"_id"`
	Version int    `json:"_version"`
	Status  int    `json:"status"`
	Error   string `json:"error"`
}

// ActionError wraps an Action we won't retry. It implements the error interface.
type ActionError struct {
	Action Action
	Msg    string
	Server string
}

func (e ActionError) Error() string {
	return fmt.Sprintf("%s: %s", e.Server, e.Msg)
}

// opt is any option to New()
type opt func(*Bubbles)

// OptConnCount is an option to New() to specify the number of connections per
// host. The default is DefaultConnCount.
func OptConnCount(n int) opt {
	return func(b *Bubbles) {
		b.connCount = n
	}
}

// OptFlush is an option to New() to specify the flush timeout of a batch. The
// default is DefaultFlushTimeout.
func OptFlush(d time.Duration) opt {
	return func(b *Bubbles) {
		b.flushTimeout = d
	}
}

// OptServerTimeout is an option to New() to specify the timeout of a single
// batch POST to ES. This value is also the maximum time Stop() will take. All
// actions in a bulk which is timed out will be retried. The default is
// DefaultServerTimeout.
func OptServerTimeout(d time.Duration) opt {
	return func(b *Bubbles) {
		b.serverTimeout = d
	}
}

// OptMaxDocs is an option to New() to specify maximum number of documents in a
// single batch. The default is DefaultMaxDocumentsPerBatch.
func OptMaxDocs(n int) opt {
	return func(b *Bubbles) {
		b.maxDocumentCount = n
	}
}

// New makes a new ES bulk inserter. It needs a list with 'ip:port' addresses,
// options are added via the Opt* functions. Be sure to read the Errors()
// channel.
func New(addrs []string, opts ...opt) *Bubbles {
	b := Bubbles{
		q:                make(chan Action),
		error:            make(chan ActionError, 10),
		quit:             make(chan struct{}),
		maxDocumentCount: DefaultMaxDocumentsPerBatch,
		connCount:        DefaultConnCount,
		flushTimeout:     DefaultFlushTimeout,
		serverTimeout:    DefaultServerTimeout,
	}
	for _, o := range opts {
		o(&b)
	}
	b.retryQ = make(chan Action, len(addrs)*b.connCount)

	// Start a go routine per connection per host
	for _, a := range addrs {
		for i := 0; i < b.connCount; i++ {
			b.wg.Add(1)
			go func(a string) {
				client(&b, a)
				b.wg.Done()
			}(a)
		}
	}
	return &b
}

// Errors returns a channel with all actions we won't retry.
func (b *Bubbles) Errors() <-chan ActionError {
	return b.error
}

// Enqueue returns the queue to add Actions in a routine. It will block if all bulk
// processors are busy.
func (b *Bubbles) Enqueue() chan<- Action {
	return b.q
}

// Stop shuts down all ES clients. It'll return all Action entries which were
// not yet processed, or were up for a retry. It can take OptServerTimeout to
// complete.
func (b *Bubbles) Stop() []Action {
	close(b.quit)
	// There is no explicit timeout, we rely on b.serverTimeout to shut down
	// everything.
	b.wg.Wait()

	close(b.error)

	// Collect and return elements which are in flight.
	close(b.retryQ)
	close(b.q)
	pending := make([]Action, 0, len(b.q)+len(b.retryQ))
	for a := range b.q {
		pending = append(pending, a)
	}
	for a := range b.retryQ {
		pending = append(pending, a)
	}
	return pending
}

// client talks to ES. This runs in a go routine in a loop and deals with a
// single ES address.
func client(b *Bubbles, addr string) {
	url := fmt.Sprintf("http://%s/_bulk", addr) // TODO: https?
	// log.Printf("starting client to %s\n", addr)
	// defer log.Printf("stopping client to %s\n", addr)

	cl := http.Client{
		Timeout:       b.serverTimeout,
		CheckRedirect: noRedirect,
	}

	for {
		select {
		case <-b.quit:
			return
		default:
		}
		if err := runBatch(b, cl, url); err != nil {
			// runBatch only returns an error on server error.
			log.Printf("server error: %s", err)
			select {
			case <-b.quit:
			case <-time.After(serverErrorWait):
				// TODO: backoff period
			}
		}
	}

}

// runBatch gathers and deals with a batch of actions. It'll return a non-nil
// error if the whole server gave an error.
func runBatch(b *Bubbles, cl http.Client, url string) error {
	actions := make([]Action, 0, b.maxDocumentCount)
	// First use all retry actions.
retry:
	for len(actions) < b.maxDocumentCount {
		select {
		case a := <-b.retryQ:
			actions = append(actions, a)
		default:
			// no more retry actions queued
			break retry
		}
	}

	var t <-chan time.Time
gather:
	for len(actions) < b.maxDocumentCount {
		if t == nil && len(actions) > 0 {
			// Set timeout on the first element we read
			t = time.After(b.flushTimeout)
		}
		select {
		case <-b.quit:
			for _, a := range actions {
				b.retryQ <- a
			}
			return nil
		case <-t:
			// this case is not enabled until we've got an action
			break gather
		case a := <-b.retryQ:
			actions = append(actions, a)
		case a := <-b.q:
			actions = append(actions, a)
		}
	}
	if len(actions) == 0 {
		// no actions. Weird.
		return nil
	}

	res, err := postActions(cl, url, actions)
	if err != nil {
		// A server error. Retry these actions later.
		for _, a := range actions {
			b.retryQ <- a
		}
		return err
	}

	// Server has accepted the request an sich, but there can be errors in the
	// individual actions.
	if !res.Errors {
		// Simple case, no errors present.
		return nil
	}
	// Figure out which actions have errors.
	for i, e := range res.Items {
		a := actions[i] // TODO: sanity check
		el, ok := e[string(a.Type)]
		if !ok {
			// TODO: this
			fmt.Printf("Non matching action!\n")
			continue
		}

		c := el.Status
		switch {
		case c >= 200 && c < 300:
			// Document accepted by ES.
		case c >= 400 && c < 500:
			// Some error. Nothing we can do with it.
			b.error <- ActionError{
				Action: a,
				Msg:    fmt.Sprintf("error %d: %s", c, el.Error),
				Server: url,
			}
		case c >= 500 && c < 600:
			// Server error. Retry it.
			b.retryQ <- a
		default:
			// No idea.
			fmt.Printf("unexpected status: %d. Ignoring document.\n", c)
		}
	}
	return nil
}

func postActions(cl http.Client, url string, actions []Action) (*bulkRes, error) {
	// TODO: bytestring as argument
	// TODO: don't chunk.
	buf := bytes.Buffer{}
	for _, a := range actions {
		buf.Write(a.Buf())
	}

	resp, err := cl.Post(url, "application/json", &buf)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var bulk bulkRes
	if err := json.Unmarshal(body, &bulk); err != nil {
		return nil, err
	}
	return &bulk, nil
}

func noRedirect(req *http.Request, via []*http.Request) error {
	return errors.New("no redirect")
}