package gateway

// A translated reply whose stream the upstream cut off mid-way — the
// connection lost, an error out of nowhere (vk-relay's relays drop Kimi
// and DeepSeek streams mid-reply every so often) — is asked of the same
// conversation again, with what the client already has of the reply sent
// back for the model to go on from: the client reads one reply that
// finished, not one cut short, and the turn doesn't fail the way it used
// to. Only a reply no tool call of has begun goes on: a call's arguments
// can't be prefilled, so one begun ends the reply as it used to. A
// refusal isn't asked again.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// streamRetries is how many times a cut reply is asked to go on before it
// ends with the error, as it used to at once.
const streamRetries = 2

// errStreamCut ends a reply's read once its error event is in hand,
// without waiting on a vendor that keeps the connection open after it.
var errStreamCut = errors.New("stream cut mid-reply")

// continuation is what of a translated reply the client already has, so a
// cut reply's next try asks the model to go on from there and only what
// is new goes to the client.
type continuation struct {
	think, text strings.Builder // thinking and text the client has
	tools       bool            // a tool call was begun: the reply can't go on
	resume      bool            // this try is a continuation
	echo        string          // the text it was prefilled with: a full echo of it is dropped
	seen        string          // what of a possible echo has come
}

// possible says whether the reply can go on: the client has something of
// it, and no tool call of it was begun.
func (c *continuation) possible() bool {
	return !c.tools && (c.think.Len() > 0 || c.text.Len() > 0)
}

// again says whether the cut reply goes on with another try, and readies
// it: not the vendor's refusal (code), nor the request itself turned away
// (a 400 the same ask gets again), the tries not up, the client still
// there.
func (c *continuation) again(status int, code string, again int, ctx context.Context) bool {
	if code != "" || status == http.StatusBadRequest || status == http.StatusUnprocessableEntity ||
		again >= streamRetries || ctx.Err() != nil || !c.possible() {
		return false
	}
	c.resume, c.echo, c.seen = true, c.text.String(), ""
	return true
}

// request is orig with what the client has of the reply sent back as the
// last message, for the model to go on from.
func (c *continuation) request(orig *Request) *Request {
	r := *orig
	var parts []Part
	if s := c.think.String(); s != "" {
		parts = append(parts, Part{Kind: Thinking, Text: s})
	}
	if s := c.text.String(); s != "" {
		parts = append(parts, Part{Kind: Text, Text: s})
	}
	r.Messages = append(slices.Clone(orig.Messages), Message{Role: "assistant", Parts: parts})
	r.Resume = true
	return &r
}

// emit hands an event of the reply to the client, less what a
// continuation says again: its framing is the encoder's already, its
// thinking isn't shown a second time, and a full echo of the text it was
// prefilled with is dropped. What goes to the client is journaled, for a
// next try to go on from.
func (c *continuation) emit(enc streamEncoder, ev Event) {
	if c.resume {
		switch ev.Kind {
		case KThink, KSig:
			return
		case KText:
			if ev.Text = c.unecho(ev.Text); ev.Text == "" {
				return
			}
		}
	}
	c.add(ev)
	enc.event(ev)
}

// add journals an event the client has.
func (c *continuation) add(ev Event) {
	switch ev.Kind {
	case KThink:
		c.think.WriteString(ev.Text)
	case KText:
		c.text.WriteString(ev.Text)
	case KToolStart, KToolArgs:
		c.tools = true
	}
}

// unecho drops a continuation's full echo of the text it was prefilled
// with, holding what could still be one until it reads either way. What
// the model says instead of echoing goes whole.
func (c *continuation) unecho(s string) string {
	if c.echo == "" {
		return s
	}
	c.seen += s
	if len(c.seen) < len(c.echo) && strings.HasPrefix(c.echo, c.seen) {
		return "" // could still be the echo
	}
	if strings.HasPrefix(c.seen, c.echo) {
		s = c.seen[len(c.echo):]
	} else {
		s = c.seen
	}
	c.seen, c.echo = "", ""
	return s
}

// streamTranslated relays a translated reply's events to the client as
// they come. A reply the upstream's stream cut off mid-way is asked of
// the same conversation again, going on from what the client has, up to
// streamRetries times; one that can't go on ends with the error in the
// client's own protocol, as it used to.
func (s *Server) streamTranslated(w http.ResponseWriter, r *http.Request, p provider.Provider, from, to provider.Protocol, request *Request, model string, zen *zenReply, u *Usage) (int, string) {
	sw := newSSEWriter(w)
	enc := encoder(from, sw, request)
	cont := &continuation{}
	var failed, failedCode string
	var failedStatus int
	var cut, errSent bool
	emit := func(ev Event) {
		switch ev.Kind {
		case KError:
			if ev.Code == "" && cont.possible() {
				// held while the reply may yet go on; sent when it can't
				failed, failedCode, failedStatus, cut = ev.Text, ev.Code, ev.Status, true
				return
			}
			failed, failedCode, failedStatus = ev.Text, ev.Code, ev.Status
			errSent = true
		case KStart, KUsage:
			u.add(ev.Usage)
			u.add(Usage{Served: ev.Model}) // the model the vendor says answered
		}
		cont.emit(enc, ev)
	}
	see := zenSee(zen, emit)
	alive := func() {
		// the provider's keepalives aren't events to translate: while it
		// is heard from, the client hears from magpie (#436)
		if failed == "" && sw.quiet() >= keepaliveGap {
			enc.keepalive()
		}
	}
	var serr error
	for again := 0; ; again++ {
		failed, failedCode, failedStatus, cut, serr = "", "", 0, false, nil
		req := request
		if cont.resume {
			req = cont.request(request)
		}
		res, actual, err := s.forwardTranslated(r.Context(), p, to, req, model, r.Header)
		if err != nil {
			if !cont.resume {
				return writeError(w, from, 502, p.Name+": "+err.Error()), err.Error()
			}
			serr = err
		}
		if err == nil {
			u.RequestID = requestID(res.Header)
		}
		if err == nil && res.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			failed, failedStatus = p.Explain(p.Name+": "+provider.APIError(b, res.Status), res.StatusCode, b), res.StatusCode
			if !cont.resume {
				if p.Preset == "openrouter" && openRouterSharedPool(b) {
					markOpenRouterSharedPool(w)
				}
				keepRetry(w.Header(), res.Header, b)
				u.ErrType = provider.ErrorType(b)
				return writeError(w, from, res.StatusCode, failed), failed
			}
		}
		if err == nil && res.StatusCode < 400 {
			rd, sse := eventStream(res)
			if !sse {
				// the provider ignored stream:true; read the whole reply as
				// one event stream would be wrong, so give up cleanly
				b, _ := io.ReadAll(io.LimitReader(rd, 1<<20))
				res.Body.Close()
				failed = p.Name + " did not stream: " + provider.APIError(b, "unexpected reply")
				if !cont.resume {
					return writeError(w, from, 502, failed), failed
				}
			} else {
				dec := decoder(actual)
				serr = readSSEAlive(rd, func(_, data string) error {
					if err := dec(data, see); err != nil {
						return err
					}
					if cut {
						return errStreamCut
					}
					return nil
				}, alive)
				res.Body.Close()
				if errors.Is(serr, errStreamCut) {
					serr = nil
				}
			}
		}
		if serr == nil && failed == "" {
			if zen != nil {
				zen.end(enc.event)
			}
			enc.finish()
			return 200, ""
		}
		if !cont.again(failedStatus, failedCode, again, r.Context()) {
			break
		}
		enc.keepalive() // the client waits while the same conversation is asked again
		select {
		case <-time.After(retryPause << again):
		case <-r.Context().Done():
		}
	}
	if serr != nil && failed == "" {
		// the upstream died mid-reply: say so in the client's own
		// protocol instead of finishing as if all went well
		failed = cutMidReply(p.Name, serr)
	}
	if !errSent {
		enc.event(Event{Kind: KError, Text: failed, Code: failedCode})
	}
	return 200, failed
}
