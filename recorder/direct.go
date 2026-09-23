package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/techbridgeinnovation/agentpulse/recorder/pb/metering"
)

// Recording a model call made outside an agent, from where the product's model client is created.
//
// An agent's calls are recorded by the framework's callbacks, which see every call without a line of the product's code changing. A service that calls a model directly has no such framework, so the only other way is a line after every call — and a call site that forgets it spends money nothing sees. This records from the client instead: set once where the client is built, every call through it is recorded, including the ones written after.
//
// What is read is the reply as the caller reads it, and only the fields that say what the call cost and how it ended: the model, the token counts, the finish, the failure. Every byte reaches the caller exactly as it arrived and nothing is held beyond what those fields need. Prompts and answers are never recorded, and a reply this cannot read is still delivered and still recorded, without its counts.

// Middleware is the shape the OpenAI and Anthropic Go sdks take for `option.WithMiddleware`. Both sdks declare theirs as an alias of exactly this, so the value is accepted by either without this library importing them.
type Middleware = func(req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error)

// The bounds on what is held while a reply is read. A reply longer than the whole is read from its two ends, which is where every provider puts the model and the counts.
const (
	maxWholeReply = 1 << 20
	replyHeadSize = 16 << 10
	replyTailSize = 256 << 10
	maxEventSize  = 1 << 20
	maxErrorReply = 64 << 10
)

// protocol is what differs between one provider's api and another's: which requests are model calls, and where a reply says what the call cost.
type protocol interface {
	// call says whether a request is a model call worth recording, and the model it names where it names one.
	call(req *http.Request) (recordable bool, model string)
	// format is the convention the counts are reported in.
	format(req *http.Request, billedBy string) string
	// billedBy is who bills for a call, where the adopter did not say.
	billedBy(req *http.Request) string
	// reply reads a whole reply's top-level fields.
	reply(field func(string) json.RawMessage, o *observed)
	// event reads one event of a streamed reply.
	event(data []byte, o *observed)
	// denied is what the caller receives for a call a spend limit refused before it was sent.
	denied(req *http.Request) (*http.Response, error)
	// failure reads a reply that refused the call.
	failure(body []byte, status int, header http.Header, billedBy string) ReportedFailure
}

// observed is what one call said about itself.
type observed struct {
	model    string
	reported map[string]int64
	tier     string
	finish   string

	// failed is a call the provider refused or that failed mid-reply, and failure what it said about it.
	failed  bool
	code    string
	failure ReportedFailure
}

// setReported replaces what a reply said it counted. A streamed reply states its counts more than once, each a running total, so the last one stated is the one that stands.
func (o *observed) setReported(usage json.RawMessage) {
	counts := flattenCounts(usage)
	if len(counts) == 0 {
		return
	}
	if o.reported == nil {
		o.reported = map[string]int64{}
	}
	for name, n := range counts {
		o.reported[name] = n
	}
}

// observe records one call made through an instrumented client, and changes nothing about it.
func (rp *Reporter) observe(p protocol, req *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if rp == nil {
		return next(req)
	}
	recordable, requested := safeCall(p, req)
	if !recordable {
		return next(req)
	}

	ctx := req.Context()
	start := time.Now()
	component := ComponentFrom(ctx)
	if component == "" {
		component = callerOf()
	}
	billedBy := rp.attribution.BilledBy
	//nolint:staticcheck // the enum is read only to keep a service configured before BilledBy existed attributing correctly
	if billedBy == "" && rp.attribution.Provider == pb.Activity_PROVIDER_UNSPECIFIED {
		billedBy = p.billedBy(req)
	}
	billedBy = BilledByOf(billedBy, rp.attribution.Provider) //nolint:staticcheck // see above
	if !rp.allowed(ctx) {
		rp.recordDenied(ctx, component, requested, billedBy)
		return p.denied(req)
	}

	c := &call{
		rp: rp, p: p, ctx: ctx, start: start, component: component, billedBy: billedBy,
		format:  p.format(req, billedBy),
		attempt: attemptOf(req),
	}
	c.o.model = requested

	resp, err := next(req)
	if err != nil {
		c.o.failed = true
		c.o.code = ErrorCode(err)
		c.o.failure = ReportedErrorFor(err, billedBy)
		c.finish()
		return resp, err
	}
	if resp == nil || resp.Body == nil {
		c.finish()
		return resp, err
	}

	c.status = resp.StatusCode
	c.header = resp.Header
	c.stream = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	// A body still compressed is one this cannot read without decompressing it a second time. Delivered untouched and recorded without its counts.
	c.unreadable = resp.Header.Get("Content-Encoding") != ""
	resp.Body = c.wrap(resp.Body)
	return resp, nil
}

// safeCall asks a protocol whether a request is a model call, and treats one that panics as not.
func safeCall(p protocol, req *http.Request) (recordable bool, model string) {
	defer func() {
		if recover() != nil {
			recordable, model = false, ""
		}
	}()
	return p.call(req)
}

// call is one model call in flight.
type call struct {
	rp        *Reporter
	p         protocol
	ctx       context.Context
	start     time.Time
	component string
	billedBy  string
	format    string
	attempt   int32

	status     int
	header     http.Header
	stream     bool
	unreadable bool

	mu       sync.Mutex
	done     bool
	whole    []byte
	head     []byte
	tail     []byte
	overflow bool
	line     []byte
	skipping bool
	o        observed
	stop     func() bool
}

// wrap puts an observer in front of a reply body. The call is recorded once, when the caller reads to the end or closes it, or when the caller's context ends with the reply abandoned part way.
func (c *call) wrap(body io.ReadCloser) io.ReadCloser {
	c.stop = context.AfterFunc(c.ctx, c.finish)
	return &observedBody{ReadCloser: body, c: c}
}

type observedBody struct {
	io.ReadCloser
	c *call
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.c.see(p[:n])
	}
	if err == io.EOF {
		b.c.finish()
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	b.c.finish()
	return err
}

// see takes a piece of the reply as the caller reads it. It never fails and never holds more than the bounds allow.
func (c *call) see(p []byte) {
	defer func() { _ = recover() }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.unreadable {
		return
	}
	switch {
	case c.status >= 400:
		c.whole = appendBounded(c.whole, p, maxErrorReply)
	case c.stream:
		c.seeLines(p)
	default:
		c.seeWhole(p)
	}
}

func (c *call) seeWhole(p []byte) {
	if !c.overflow && len(c.whole)+len(p) <= maxWholeReply {
		c.whole = append(c.whole, p...)
		return
	}
	if !c.overflow {
		c.overflow = true
		c.head = append([]byte(nil), c.whole[:min(len(c.whole), replyHeadSize)]...)
		c.tail = c.whole
		c.whole = nil
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 2*replyTailSize {
		c.tail = append([]byte(nil), c.tail[len(c.tail)-replyTailSize:]...)
	}
}

// seeLines reads a streamed reply one event line at a time. A line longer than any event carrying counts is skipped rather than held.
func (c *call) seeLines(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !c.skipping {
				if len(c.line)+len(p) > maxEventSize {
					c.line, c.skipping = c.line[:0], true
				} else {
					c.line = append(c.line, p...)
				}
			}
			return
		}
		if !c.skipping && len(c.line)+i <= maxEventSize {
			c.line = append(c.line, p[:i]...)
			c.readLine(c.line)
		}
		c.line, c.skipping = c.line[:0], false
		p = p[i+1:]
	}
}

func (c *call) readLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	c.p.event(data, &c.o)
}

// finish records the call, once.
func (c *call) finish() {
	defer func() { _ = recover() }()
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	if c.stop != nil {
		c.stop()
	}
	c.read()
	o := c.o
	c.whole, c.head, c.tail, c.line = nil, nil, nil, nil
	c.mu.Unlock()

	c.rp.recordObserved(c, o)
}

// read makes what was held into what the call said, for a reply that has been read as far as it will be.
func (c *call) read() {
	if c.unreadable {
		return
	}
	switch {
	case c.status >= 400:
		c.o.failed = true
		c.o.failure = c.p.failure(c.whole, c.status, c.header, c.billedBy)
		c.o.code = firstOf(codeOf(c.o.failure), "HTTP_"+strconv.Itoa(c.status))
	case c.stream:
		if len(c.line) > 0 && !c.skipping {
			c.readLine(c.line)
		}
	case c.overflow:
		c.p.reply(func(name string) json.RawMessage {
			if v := lastValue(c.tail, name); v != nil {
				return v
			}
			return firstValue(c.head, name)
		}, &c.o)
	case len(c.whole) > 0:
		var fields map[string]json.RawMessage
		if json.Unmarshal(c.whole, &fields) == nil {
			c.p.reply(func(name string) json.RawMessage { return fields[name] }, &c.o)
		}
	}
}

// recordObserved turns what a call said about itself into a record.
func (rp *Reporter) recordObserved(c *call, o observed) {
	activity := rp.activity(c.ctx, c.component, time.Since(c.start), nil, nil)
	activity.Model = o.model
	activity.BilledBy = c.billedBy
	activity.Attempt = c.attempt
	activity.ServiceTier = o.tier
	if len(o.reported) > 0 {
		activity.UsageFormat = c.format
		activity.ReportedUsage = reportedQuantities(o.reported)
	}

	switch {
	case o.failed:
		activity.Status = pb.Activity_FAILED
		activity.ErrorCode = o.code
		activity.ErrorFormat = o.failure.Format
		activity.ReportedError = o.failure.Fields
	case truncatedFinish(o.finish):
		activity.Status = pb.Activity_TRUNCATED
		activity.ErrorCode = o.finish
	case BlockedFinish(o.finish):
		activity.Status = pb.Activity_FAILED
		activity.ErrorCode = o.finish
	}
	rp.recorder.RecordIn(c.ctx, activity)
}

// attemptOf is which attempt at one call a request is, where the sdk says. The OpenAI and Anthropic sdks number their retries on every attempt they send.
func attemptOf(req *http.Request) int32 {
	n, err := strconv.Atoi(req.Header.Get("X-Stainless-Retry-Count"))
	if err != nil || n < 0 {
		return 0
	}
	return int32(n + 1)
}

// flattenCounts reads a provider's usage object into its counts, each under the provider's own name, with a nested count named by its path, e.g. `prompt_tokens_details.cached_tokens`. Anything that is not a whole number is left out.
func flattenCounts(raw json.RawMessage) map[string]int64 {
	var usage map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	out := map[string]int64{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for name, value := range m {
			switch v := value.(type) {
			case float64:
				if v != 0 && v == float64(int64(v)) {
					out[prefix+name] = int64(v)
				}
			case map[string]any:
				walk(prefix+name+".", v)
			}
		}
	}
	walk("", usage)
	return out
}

// appendBounded appends as much of p as fits under limit.
func appendBounded(buf, p []byte, limit int) []byte {
	if room := limit - len(buf); room > 0 {
		return append(buf, p[:min(room, len(p))]...)
	}
	return buf
}

// lastValue is the value of the last top-level-looking `"name":` in a piece of JSON, for a reply read from its end. A key inside a string cannot match, because a quote inside a string is escaped.
func lastValue(buf []byte, name string) json.RawMessage {
	key := []byte(`"` + name + `":`)
	for end := len(buf); end > 0; {
		i := bytes.LastIndex(buf[:end], key)
		if i < 0 {
			return nil
		}
		if i == 0 || buf[i-1] != '\\' {
			return valueAt(buf[i+len(key):])
		}
		end = i
	}
	return nil
}

// firstValue is the value of the first `"name":` in a piece of JSON, for a reply read from its start.
func firstValue(buf []byte, name string) json.RawMessage {
	key := []byte(`"` + name + `":`)
	for start := 0; start < len(buf); {
		i := bytes.Index(buf[start:], key)
		if i < 0 {
			return nil
		}
		i += start
		if i == 0 || buf[i-1] != '\\' {
			return valueAt(buf[i+len(key):])
		}
		start = i + len(key)
	}
	return nil
}

// valueAt reads one JSON value from the start of buf, and is nothing where the value does not end inside it.
func valueAt(buf []byte) json.RawMessage {
	buf = bytes.TrimLeft(buf, " \t\r\n")
	if len(buf) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	var v json.RawMessage
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// callerOf names the function in the product's own code that made a model call, for a call made with no component named: the first frame outside the standard library, this library, the model sdks and the transports they sit on.
func callerOf() string {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if !infrastructure(frame.Function) {
			return shortName(frame.Function)
		}
		if !more {
			return ""
		}
	}
}

// ownPackage is this library's import path, whichever module it is published under.
var ownPackage = func() string {
	name := runtime.FuncForPC(reflectPC()).Name()
	return name[:strings.LastIndex(name, ".")]
}()

func reflectPC() uintptr {
	pc, _, _, _ := runtime.Caller(0)
	return pc
}

// infrastructurePrefixes are the code a model call passes through on its way out that is never the product's own.
var infrastructurePrefixes = []string{
	"runtime.", "net/http.", "net.", "crypto/", "iter.", "io.", "context.", "sync.",
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/openai/openai-go",
	"google.golang.org/genai",
	"google.golang.org/grpc",
	"cloud.google.com/go/auth",
	"golang.org/x/oauth2",
	"go.opentelemetry.io/",
}

func infrastructure(function string) bool {
	if function == "" || strings.HasPrefix(function, ownPackage+".") {
		return true
	}
	for _, prefix := range infrastructurePrefixes {
		if strings.HasPrefix(function, prefix) {
			return true
		}
	}
	return false
}

// shortName is a function's name as a component: its package and name, or its type and method, without the path it was imported from or the closures inside it, e.g. `summary.Generate`, `PromptsService.GeneratePrompt`.
func shortName(function string) string {
	name := function[strings.LastIndex(function, "/")+1:]
	parts := strings.Split(name, ".")
	for len(parts) > 1 && closure(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	if parts[0] == "main" && len(parts) > 1 {
		parts = parts[1:]
	}
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(p, "("), "*"), ")")
	}
	return strings.Join(parts, ".")
}

// closure is a name the compiler gives a function literal, e.g. `func1`, `gowrap2`, or a numbered inner closure, e.g. `1`.
func closure(part string) bool {
	for _, prefix := range []string{"func", "gowrap", "deferwrap"} {
		if rest, ok := strings.CutPrefix(part, prefix); ok && digits(rest) {
			return true
		}
	}
	return digits(part)
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
