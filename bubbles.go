package bubbles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultMaxDocumentsPerBatch is the number of documents a batch needs to
	// have before it's send. This is per connection.
	DefaultMaxDocumentsPerBatch = 10

	// DefaultFlushTimeout is the maximum time we batch something before we try
	// to send it to a server.
	DefaultFlushTimeout = 10 * time.Second

	// DefaultCPH is the number of connections per hosts.
	DefaultCPH = 2

	defaultServerErrorWait = 1 * time.Second
)

type bubbles struct {
	q                chan *Action
	retryQ           chan *Action
	error            chan ActionError
	quit             chan struct{}
	wg               sync.WaitGroup
	maxDocumentCount int
	serverErrorWait  time.Duration
	flushTimeout     time.Duration
	cph              int
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
type opt func(*bubbles)

// OptConnCount is an option to New() to specify the number of connections per
// host. The default is DefaultCPH.
func OptConnCount(n int) opt {
	return func(b *bubbles) {
		b.cph = n
	}
}

// OptFlush is an option to New() to specify the flush timeout of a batch. The
// default is DefaultFlushTimeout.
func OptFlush(d time.Duration) opt {
	return func(b *bubbles) {
		b.flushTimeout = d
	}
}

// OptMaxDocs is an option to New() to specify maximum number of documents in a
// single batch. The  default is DefaultMaxDocumentsPerBatch.
func OptMaxDocs(n int) opt {
	return func(b *bubbles) {
		b.maxDocumentCount = n
	}
}

// New makes a new ES bulk inserter. It needs a list with 'ip:port' addresses,
// options are added via the Opt* functions. Be sure to read the Errors()
// channel.
func New(addrs []string, opts ...opt) (*bubbles, error) {
	b := bubbles{
		q:                make(chan *Action),
		error:            make(chan ActionError, 1),
		quit:             make(chan struct{}),
		maxDocumentCount: DefaultMaxDocumentsPerBatch,
		serverErrorWait:  defaultServerErrorWait,
		flushTimeout:     DefaultFlushTimeout,
		cph:              DefaultCPH,
	}
	for _, o := range opts {
		o(&b)
	}
	b.retryQ = make(chan *Action, len(addrs)*b.cph)

	// Start a go routine per connection per host
	for _, a := range addrs {
		for i := 0; i < b.cph; i++ {
			b.wg.Add(1)
			go func(a string) {
				client(&b, a)
				b.wg.Done()
			}(a)
		}
	}
	return &b, nil
}

// Errors returns a channel with all actions we won't retry.
func (b *bubbles) Errors() <-chan ActionError {
	return b.error
}

// Enqueue queues a single action in a routine. It will block if all bulk
// processers are busy.
func (b bubbles) Enqueue(a *Action) {
	b.q <- a
}

// Stop shuts down all ES clients. It'll return all Action entries which were
// not yet processed, or were up for a retry.
func (b *bubbles) Stop() []*Action {
	close(b.quit)
	// TODO: timeout
	b.wg.Wait()

	// Collect and return elements which are in flight.
	close(b.retryQ) // after Wait()!
	close(b.q)      // after Wait()!
	pending := make([]*Action, 0, len(b.retryQ)+len(b.q))
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
func client(b *bubbles, addr string) {
	url := fmt.Sprintf("http://%s/_bulk", addr) // TODO: https?
	// fmt.Printf("starting client to %s\n", addr)
	// defer fmt.Printf("stopping client to %s\n", addr)

	cl := http.Client{
		Transport: &http.Transport{
			DisableCompression: true, // TODO: sure?
		},
		CheckRedirect: noRedirect,
	}

	for {
		select {
		case <-b.quit:
			return
		default:
		}
		runBatch(b, cl, url)
	}

}

func runBatch(b *bubbles, cl http.Client, url string) {
	actions := make([]*Action, 0, b.maxDocumentCount)
	var t <-chan time.Time
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

gather:
	for len(actions) < b.maxDocumentCount {
		if actions != nil && t == nil {
			// Set timeout on the first element we read
			t = time.After(b.flushTimeout)
		}
		select {
		case <-b.quit:
			for _, a := range actions {
				b.retryQ <- a
			}
			return
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
		return
	}

	res, err := postActions(cl, url, actions)
	if err != nil {
		// A server error. Put all elements back in the queue and wait a bit.
		for _, a := range actions {
			b.retryQ <- a
		}

		select {
		case <-b.quit:
		case <-time.After(b.serverErrorWait):
		}
		return
	}

	// Server has accepted the request, but there can be individual errors.
	if !res.Errors {
		// Simple case, no errors.
		return
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
				Action: *a,
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
}

func postActions(cl http.Client, url string, actions []*Action) (*bulkRes, error) {
	// TODO: bytestring as argument
	// TODO: don't chunk.
	// TODO: timeout
	buf := bytes.Buffer{}
	for _, a := range actions {
		buf.Write(a.Buf())
	}

	resp, err := cl.Post(url, "application/x-www-form-urlencoded", &buf)
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
