// Copyright 2020-2021 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Request API subjects for JetStream.
const (
	// defaultAPIPrefix is the default prefix for the JetStream API.
	defaultAPIPrefix = "$JS.API."

	// apiAccountInfo is for obtaining general information about JetStream.
	apiAccountInfo = "INFO"

	// apiConsumerCreateT is used to create consumers.
	apiConsumerCreateT = "CONSUMER.CREATE.%s"

	// apiDurableCreateT is used to create durable consumers.
	apiDurableCreateT = "CONSUMER.DURABLE.CREATE.%s.%s"

	// apiConsumerInfoT is used to create consumers.
	apiConsumerInfoT = "CONSUMER.INFO.%s.%s"

	// apiRequestNextT is the prefix for the request next message(s) for a consumer in worker/pull mode.
	apiRequestNextT = "CONSUMER.MSG.NEXT.%s.%s"

	// apiDeleteConsumerT is used to delete consumers.
	apiConsumerDeleteT = "CONSUMER.DELETE.%s.%s"

	// apiConsumerListT is used to return all detailed consumer information
	apiConsumerListT = "CONSUMER.LIST.%s"

	// apiConsumerNamesT is used to return a list with all consumer names for the stream.
	apiConsumerNamesT = "CONSUMER.NAMES.%s"

	// apiStreams can lookup a stream by subject.
	apiStreams = "STREAM.NAMES"

	// apiStreamCreateT is the endpoint to create new streams.
	apiStreamCreateT = "STREAM.CREATE.%s"

	// apiStreamInfoT is the endpoint to get information on a stream.
	apiStreamInfoT = "STREAM.INFO.%s"

	// apiStreamUpdate is the endpoint to update existing streams.
	apiStreamUpdateT = "STREAM.UPDATE.%s"

	// apiStreamDeleteT is the endpoint to delete streams.
	apiStreamDeleteT = "STREAM.DELETE.%s"

	// apiPurgeStreamT is the endpoint to purge streams.
	apiStreamPurgeT = "STREAM.PURGE.%s"

	// apiStreamListT is the endpoint that will return all detailed stream information
	apiStreamList = "STREAM.LIST"

	// apiMsgGetT is the endpoint to get a message.
	apiMsgGetT = "STREAM.MSG.GET.%s"

	// apiMsgDeleteT is the endpoint to remove a message.
	apiMsgDeleteT = "STREAM.MSG.DELETE.%s"
)

// JetStream is the public interface for JetStream.
type JetStream interface {
	// Publish publishes a message to JetStream.
	Publish(subj string, data []byte, opts ...PubOpt) (*PubAck, error)

	// Publish publishes a Msg to JetStream.
	PublishMsg(m *Msg, opts ...PubOpt) (*PubAck, error)

	// Subscribe creates an async Subscription for JetStream.
	Subscribe(subj string, cb MsgHandler, opts ...SubOpt) (*Subscription, error)

	// SubscribeSync creates a Subscription that can be used to process messages synchronously.
	SubscribeSync(subj string, opts ...SubOpt) (*Subscription, error)

	// ChanSubscribe creates channel based Subscription.
	ChanSubscribe(subj string, ch chan *Msg, opts ...SubOpt) (*Subscription, error)

	// QueueSubscribe creates a Subscription with a queue group.
	QueueSubscribe(subj, queue string, cb MsgHandler, opts ...SubOpt) (*Subscription, error)

	// QueueSubscribeSync creates a Subscription with a queue group that can be used to process messages synchronously.
	QueueSubscribeSync(subj, queue string, opts ...SubOpt) (*Subscription, error)

	// PullSubscribe creates a Subscription that can fetch messages.
	PullSubscribe(subj string, opts ...SubOpt) (*Subscription, error)
}

// JetStreamContext is the public interface for JetStream.
type JetStreamContext interface {
	JetStream
	JetStreamManager
}

// js is an internal struct from a JetStreamContext.
type js struct {
	nc   *Conn
	opts *jsOpts
}

type jsOpts struct {
	ctx context.Context
	// For importing JetStream from other accounts.
	pre string
	// Amount of time to wait for API requests.
	wait time.Duration
	// Signals only direct access and no API access.
	direct bool
}

const defaultRequestWait = 5 * time.Second

// JetStream returns a JetStream context for pub/sub interactions.
func (nc *Conn) JetStream(opts ...JSOpt) (JetStreamContext, error) {
	js := &js{
		nc: nc,
		opts: &jsOpts{
			pre:  defaultAPIPrefix,
			wait: defaultRequestWait,
		},
	}

	for _, opt := range opts {
		if err := opt.configureJSContext(js.opts); err != nil {
			return nil, err
		}
	}

	if js.opts.direct {
		return js, nil
	}

	if _, err := js.AccountInfo(); err != nil {
		if err == ErrNoResponders {
			err = ErrJetStreamNotEnabled
		}
		return nil, err
	}

	return js, nil
}

// JSOpt configures a JetStream context.
type JSOpt interface {
	configureJSContext(opts *jsOpts) error
}

// jsOptFn configures an option for the JetStream context.
type jsOptFn func(opts *jsOpts) error

func (opt jsOptFn) configureJSContext(opts *jsOpts) error {
	return opt(opts)
}

// APIPrefix changes the default prefix used for the JetStream API.
func APIPrefix(pre string) JSOpt {
	return jsOptFn(func(js *jsOpts) error {
		js.pre = pre
		if !strings.HasSuffix(js.pre, ".") {
			js.pre = js.pre + "."
		}
		return nil
	})
}

// DirectOnly makes a JetStream context avoid using the JetStream API altogether.
func DirectOnly() JSOpt {
	return jsOptFn(func(js *jsOpts) error {
		js.direct = true
		return nil
	})
}

func (js *js) apiSubj(subj string) string {
	if js.opts.pre == _EMPTY_ {
		return subj
	}
	var b strings.Builder
	b.WriteString(js.opts.pre)
	b.WriteString(subj)
	return b.String()
}

// PubOpt configures options for publishing JetStream messages.
type PubOpt interface {
	configurePublish(opts *pubOpts) error
}

// pubOptFn is a function option used to configure JetStream Publish.
type pubOptFn func(opts *pubOpts) error

func (opt pubOptFn) configurePublish(opts *pubOpts) error {
	return opt(opts)
}

type pubOpts struct {
	ctx context.Context
	ttl time.Duration
	id  string
	lid string // Expected last msgId
	str string // Expected stream name
	seq uint64 // Expected last sequence
}

// pubAckResponse is the ack response from the JetStream API when of publishing a message.
type pubAckResponse struct {
	apiResponse
	*PubAck
}

// PubAck is an ack received after successfully publishing a message.
type PubAck struct {
	Stream    string `json:"stream"`
	Sequence  uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// Headers for published messages.
const (
	MsgIdHdr             = "Nats-Msg-Id"
	ExpectedStreamHdr    = "Nats-Expected-Stream"
	ExpectedLastSeqHdr   = "Nats-Expected-Last-Sequence"
	ExpectedLastMsgIdHdr = "Nats-Expected-Last-Msg-Id"
)

// PublishMsg publishes a Msg to a stream from JetStream.
func (js *js) PublishMsg(m *Msg, opts ...PubOpt) (*PubAck, error) {
	var o pubOpts
	if len(opts) > 0 {
		if m.Header == nil {
			m.Header = http.Header{}
		}
		for _, opt := range opts {
			if err := opt.configurePublish(&o); err != nil {
				return nil, err
			}
		}
	}
	// Check for option collisions. Right now just timeout and context.
	if o.ctx != nil && o.ttl != 0 {
		return nil, ErrContextAndTimeout
	}
	if o.ttl == 0 && o.ctx == nil {
		o.ttl = js.opts.wait
	}

	if o.id != _EMPTY_ {
		m.Header.Set(MsgIdHdr, o.id)
	}
	if o.lid != _EMPTY_ {
		m.Header.Set(ExpectedLastMsgIdHdr, o.lid)
	}
	if o.str != _EMPTY_ {
		m.Header.Set(ExpectedStreamHdr, o.str)
	}
	if o.seq > 0 {
		m.Header.Set(ExpectedLastSeqHdr, strconv.FormatUint(o.seq, 10))
	}

	var resp *Msg
	var err error

	if o.ttl > 0 {
		resp, err = js.nc.RequestMsg(m, time.Duration(o.ttl))
	} else {
		resp, err = js.nc.RequestMsgWithContext(o.ctx, m)
	}

	if err != nil {
		if err == ErrNoResponders {
			err = ErrNoStreamResponse
		}
		return nil, err
	}
	var pa pubAckResponse
	if err := json.Unmarshal(resp.Data, &pa); err != nil {
		return nil, ErrInvalidJSAck
	}
	if pa.Error != nil {
		return nil, errors.New(pa.Error.Description)
	}
	if pa.PubAck == nil || pa.PubAck.Stream == _EMPTY_ {
		return nil, ErrInvalidJSAck
	}
	return pa.PubAck, nil
}

// Publish publishes a message to a stream from JetStream.
func (js *js) Publish(subj string, data []byte, opts ...PubOpt) (*PubAck, error) {
	return js.PublishMsg(&Msg{Subject: subj, Data: data}, opts...)
}

// MsgId sets the message ID used for de-duplication.
func MsgId(id string) PubOpt {
	return pubOptFn(func(opts *pubOpts) error {
		opts.id = id
		return nil
	})
}

// ExpectStream sets the expected stream to respond from the publish.
func ExpectStream(stream string) PubOpt {
	return pubOptFn(func(opts *pubOpts) error {
		opts.str = stream
		return nil
	})
}

// ExpectLastSequence sets the expected sequence in the response from the publish.
func ExpectLastSequence(seq uint64) PubOpt {
	return pubOptFn(func(opts *pubOpts) error {
		opts.seq = seq
		return nil
	})
}

// ExpectLastSequence sets the expected sequence in the response from the publish.
func ExpectLastMsgId(id string) PubOpt {
	return pubOptFn(func(opts *pubOpts) error {
		opts.lid = id
		return nil
	})
}

// MaxWait sets the maximum amount of time we will wait for a response.
type MaxWait time.Duration

func (ttl MaxWait) configureJSContext(js *jsOpts) error {
	js.wait = time.Duration(ttl)
	return nil
}

func (ttl MaxWait) configurePull(opts *pullOpts) error {
	opts.ttl = time.Duration(ttl)
	return nil
}

// AckWait sets the maximum amount of time we will wait for an ack.
type AckWait time.Duration

func (ttl AckWait) configurePublish(opts *pubOpts) error {
	opts.ttl = time.Duration(ttl)
	return nil
}

func (ttl AckWait) configureSubscribe(opts *subOpts) error {
	opts.cfg.AckWait = time.Duration(ttl)
	return nil
}

// ContextOpt is an option used to set a context.Context.
type ContextOpt struct {
	context.Context
}

func (ctx ContextOpt) configureJSContext(opts *jsOpts) error {
	opts.ctx = ctx
	return nil
}

func (ctx ContextOpt) configurePublish(opts *pubOpts) error {
	opts.ctx = ctx
	return nil
}

func (ctx ContextOpt) configurePull(opts *pullOpts) error {
	opts.ctx = ctx
	return nil
}

// Context returns an option that can be used to configure a context.
func Context(ctx context.Context) ContextOpt {
	return ContextOpt{ctx}
}

// Subscribe

// ConsumerConfig is the configuration of a JetStream consumer.
type ConsumerConfig struct {
	Durable         string        `json:"durable_name,omitempty"`
	DeliverSubject  string        `json:"deliver_subject,omitempty"`
	DeliverPolicy   DeliverPolicy `json:"deliver_policy"`
	OptStartSeq     uint64        `json:"opt_start_seq,omitempty"`
	OptStartTime    *time.Time    `json:"opt_start_time,omitempty"`
	AckPolicy       AckPolicy     `json:"ack_policy"`
	AckWait         time.Duration `json:"ack_wait,omitempty"`
	MaxDeliver      int           `json:"max_deliver,omitempty"`
	FilterSubject   string        `json:"filter_subject,omitempty"`
	ReplayPolicy    ReplayPolicy  `json:"replay_policy"`
	RateLimit       uint64        `json:"rate_limit_bps,omitempty"` // Bits per sec
	SampleFrequency string        `json:"sample_freq,omitempty"`
	MaxWaiting      int           `json:"max_waiting,omitempty"`
	MaxAckPending   int           `json:"max_ack_pending,omitempty"`
}

// ConsumerInfo is the info from a JetStream consumer.
type ConsumerInfo struct {
	Stream         string         `json:"stream_name"`
	Name           string         `json:"name"`
	Created        time.Time      `json:"created"`
	Config         ConsumerConfig `json:"config"`
	Delivered      SequencePair   `json:"delivered"`
	AckFloor       SequencePair   `json:"ack_floor"`
	NumAckPending  int            `json:"num_ack_pending"`
	NumRedelivered int            `json:"num_redelivered"`
	NumWaiting     int            `json:"num_waiting"`
	NumPending     uint64         `json:"num_pending"`
	Cluster        *ClusterInfo   `json:"cluster,omitempty"`
}

// SequencePair includes the consumer and stream sequence info from a JetStream consumer.
type SequencePair struct {
	Consumer uint64 `json:"consumer_seq"`
	Stream   uint64 `json:"stream_seq"`
}

// nextRequest is for getting next messages for pull based consumers from JetStream.
type nextRequest struct {
	Expires time.Duration `json:"expires,omitempty"`
	Batch   int           `json:"batch,omitempty"`
	NoWait  bool          `json:"no_wait,omitempty"`
}

// jsSub includes JetStream subscription info.
type jsSub struct {
	js       *js
	consumer string
	stream   string
	deliver  string
	pull     bool
	durable  bool
	attached bool
}

func (jsi *jsSub) unsubscribe(drainMode bool) error {
	if drainMode && (jsi.durable || jsi.attached) {
		// Skip deleting consumer for durables/attached
		// consumers when using drain mode.
		return nil
	}

	// Skip if in direct mode as well.
	js := jsi.js
	if js.opts.direct {
		return nil
	}

	return js.DeleteConsumer(jsi.stream, jsi.consumer)
}

// SubOpt configures options for subscribing to JetStream consumers.
type SubOpt interface {
	configureSubscribe(opts *subOpts) error
}

// subOptFn is a function option used to configure a JetStream Subscribe.
type subOptFn func(opts *subOpts) error

func (opt subOptFn) configureSubscribe(opts *subOpts) error {
	return opt(opts)
}

// Subscribe will create a subscription to the appropriate stream and consumer.
func (js *js) Subscribe(subj string, cb MsgHandler, opts ...SubOpt) (*Subscription, error) {
	if cb == nil {
		return nil, ErrBadSubscription
	}
	return js.subscribe(subj, _EMPTY_, cb, nil, opts)
}

// SubscribeSync will create a sync subscription to the appropriate stream and consumer.
func (js *js) SubscribeSync(subj string, opts ...SubOpt) (*Subscription, error) {
	mch := make(chan *Msg, js.nc.Opts.SubChanLen)
	return js.subscribe(subj, _EMPTY_, nil, mch, opts)
}

// QueueSubscribe will create a subscription to the appropriate stream and consumer with queue semantics.
func (js *js) QueueSubscribe(subj, queue string, cb MsgHandler, opts ...SubOpt) (*Subscription, error) {
	if cb == nil {
		return nil, ErrBadSubscription
	}
	return js.subscribe(subj, queue, cb, nil, opts)
}

// QueueSubscribeSync will create a sync subscription to the appropriate stream and consumer with queue semantics.
func (js *js) QueueSubscribeSync(subj, queue string, opts ...SubOpt) (*Subscription, error) {
	mch := make(chan *Msg, js.nc.Opts.SubChanLen)
	return js.subscribe(subj, queue, nil, mch, opts)
}

// Subscribe will create a subscription to the appropriate stream and consumer.
func (js *js) ChanSubscribe(subj string, ch chan *Msg, opts ...SubOpt) (*Subscription, error) {
	return js.subscribe(subj, _EMPTY_, nil, ch, opts)
}

// PullSubscribe creates a pull subscriber.
func (js *js) PullSubscribe(subj string, opts ...SubOpt) (*Subscription, error) {
	return js.subscribe(subj, _EMPTY_, nil, nil, opts)
}

func (js *js) subscribe(subj, queue string, cb MsgHandler, ch chan *Msg, opts []SubOpt) (*Subscription, error) {
	cfg := ConsumerConfig{AckPolicy: ackPolicyNotSet}
	o := subOpts{cfg: &cfg}
	if len(opts) > 0 {
		for _, opt := range opts {
			if err := opt.configureSubscribe(&o); err != nil {
				return nil, err
			}
		}
	}

	isPullMode := ch == nil && cb == nil
	badPullAck := o.cfg.AckPolicy == AckNonePolicy || o.cfg.AckPolicy == AckAllPolicy
	if isPullMode && badPullAck {
		return nil, fmt.Errorf("invalid ack mode for pull consumers: %s", o.cfg.AckPolicy)
	}

	var (
		err          error
		shouldCreate bool
		ccfg         *ConsumerConfig
		deliver      string
		attached     bool
		stream       = o.stream
		consumer     = o.consumer
		requiresAPI  = (stream == _EMPTY_ && consumer == _EMPTY_) && o.cfg.DeliverSubject == _EMPTY_
	)

	if js.opts.direct && requiresAPI {
		return nil, ErrDirectModeRequired
	}

	if js.opts.direct {
		if o.cfg.DeliverSubject != _EMPTY_ {
			deliver = o.cfg.DeliverSubject
		} else {
			deliver = NewInbox()
		}
	} else {
		// Find the stream mapped to the subject if not bound to a stream already.
		if o.stream == _EMPTY_ {
			stream, err = js.lookupStreamBySubject(subj)
			if err != nil {
				return nil, err
			}
		} else {
			stream = o.stream
		}

		// With an explicit durable name, then can lookup
		// the consumer to which it should be attaching to.
		var info *ConsumerInfo
		consumer = o.cfg.Durable
		if consumer != _EMPTY_ {
			// Only create in case there is no consumer already.
			info, err = js.ConsumerInfo(stream, consumer)
			if err != nil && err.Error() != `consumer not found` {
				return nil, err
			}
		}

		if info != nil {
			// Attach using the found consumer config.
			ccfg = &info.Config
			attached = true

			// Make sure this new subject matches or is a subset.
			if ccfg.FilterSubject != _EMPTY_ && subj != ccfg.FilterSubject {
				return nil, ErrSubjectMismatch
			}

			if ccfg.DeliverSubject != _EMPTY_ {
				deliver = ccfg.DeliverSubject
			} else {
				deliver = NewInbox()
			}
		} else {
			shouldCreate = true
			deliver = NewInbox()
			if !isPullMode {
				cfg.DeliverSubject = deliver
			}
			// Do filtering always, server will clear as needed.
			cfg.FilterSubject = subj
		}
	}

	var sub *Subscription

	// Check if we are manual ack.
	if cb != nil && !o.mack {
		ocb := cb
		cb = func(m *Msg) { ocb(m); m.Ack() }
	}

	if isPullMode {
		sub = &Subscription{Subject: subj, conn: js.nc, typ: PullSubscription, jsi: &jsSub{js: js, pull: true}}
	} else {
		sub, err = js.nc.subscribe(deliver, queue, cb, ch, cb == nil, &jsSub{js: js})
		if err != nil {
			return nil, err
		}
	}

	// If we are creating or updating let's process that request.
	if shouldCreate {
		// If not set default to ack explicit.
		if cfg.AckPolicy == ackPolicyNotSet {
			cfg.AckPolicy = AckExplicitPolicy
		}
		// If we have acks at all and the MaxAckPending is not set go ahead
		// and set to the internal max.
		// TODO(dlc) - We should be able to update this if client updates PendingLimits.
		if cfg.MaxAckPending == 0 && cfg.AckPolicy != AckNonePolicy {
			maxMsgs, _, _ := sub.PendingLimits()
			cfg.MaxAckPending = maxMsgs
		}

		req := &createConsumerRequest{
			Stream: stream,
			Config: &cfg,
		}

		j, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}

		var ccSubj string
		isDurable := cfg.Durable != _EMPTY_
		if isDurable {
			ccSubj = fmt.Sprintf(apiDurableCreateT, stream, cfg.Durable)
		} else {
			ccSubj = fmt.Sprintf(apiConsumerCreateT, stream)
		}

		resp, err := js.nc.Request(js.apiSubj(ccSubj), j, js.opts.wait)
		if err != nil {
			if err == ErrNoResponders {
				err = ErrJetStreamNotEnabled
			}
			sub.Unsubscribe()
			return nil, err
		}

		var info consumerResponse
		err = json.Unmarshal(resp.Data, &info)
		if err != nil {
			sub.Unsubscribe()
			return nil, err
		}
		if info.Error != nil {
			sub.Unsubscribe()
			return nil, errors.New(info.Error.Description)
		}

		// Hold onto these for later.
		sub.jsi.stream = info.Stream
		sub.jsi.consumer = info.Name
		sub.jsi.deliver = info.Config.DeliverSubject
		sub.jsi.durable = isDurable
	} else {
		sub.jsi.stream = stream
		sub.jsi.consumer = consumer
		if js.opts.direct {
			sub.jsi.deliver = o.cfg.DeliverSubject
		} else {
			sub.jsi.deliver = ccfg.DeliverSubject
		}
	}
	sub.jsi.attached = attached

	// If we are pull based go ahead and fire off the first request to populate.
	if isPullMode {
		sub.jsi.pull = o.pull
	}

	return sub, nil
}

type streamRequest struct {
	Subject string `json:"subject,omitempty"`
}

type streamNamesResponse struct {
	apiResponse
	apiPaged
	Streams []string `json:"streams"`
}

func (js *js) lookupStreamBySubject(subj string) (string, error) {
	var slr streamNamesResponse
	req := &streamRequest{subj}
	j, err := json.Marshal(req)
	if err != nil {
		return _EMPTY_, err
	}
	resp, err := js.nc.Request(js.apiSubj(apiStreams), j, js.opts.wait)
	if err != nil {
		if err == ErrNoResponders {
			err = ErrJetStreamNotEnabled
		}
		return _EMPTY_, err
	}
	if err := json.Unmarshal(resp.Data, &slr); err != nil {
		return _EMPTY_, err
	}
	if slr.Error != nil || len(slr.Streams) != 1 {
		return _EMPTY_, ErrNoMatchingStream
	}
	return slr.Streams[0], nil
}

type subOpts struct {
	// For attaching.
	stream, consumer string
	// For pull based consumers.
	pull bool
	// For manual ack
	mack bool
	// For creating or updating.
	cfg *ConsumerConfig
}

// ManualAck disables auto ack functionality for async subscriptions.
func ManualAck() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.mack = true
		return nil
	})
}

// Durable defines the consumer name for JetStream durable subscribers.
func Durable(name string) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		if strings.Contains(name, ".") {
			return ErrInvalidDurableName
		}

		opts.cfg.Durable = name
		return nil
	})
}

// DeliverAll will configure a Consumer to receive all the
// messages from a Stream.
func DeliverAll() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.DeliverPolicy = DeliverAllPolicy
		return nil
	})
}

// DeliverLast configures a Consumer to receive messages
// starting with the latest one.
func DeliverLast() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.DeliverPolicy = DeliverLastPolicy
		return nil
	})
}

// DeliverNew configures a Consumer to receive messages
// published after the subscription.
func DeliverNew() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.DeliverPolicy = DeliverNewPolicy
		return nil
	})
}

// StartSequence configures a Consumer to receive
// messages from a start sequence.
func StartSequence(seq uint64) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.DeliverPolicy = DeliverByStartSequencePolicy
		opts.cfg.OptStartSeq = seq
		return nil
	})
}

// StartTime configures a Consumer to receive
// messages from a start time.
func StartTime(startTime time.Time) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.DeliverPolicy = DeliverByStartTimePolicy
		opts.cfg.OptStartTime = &startTime
		return nil
	})
}

func AckNone() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.AckPolicy = AckNonePolicy
		return nil
	})
}

func AckAll() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.AckPolicy = AckAllPolicy
		return nil
	})
}

func AckExplicit() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.AckPolicy = AckExplicitPolicy
		return nil
	})
}

func MaxDeliver(n int) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.MaxDeliver = n
		return nil
	})
}

func MaxAckPending(n int) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.MaxAckPending = n
		return nil
	})
}

// ReplayOriginal replays the messages at the original speed.
func ReplayOriginal() SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.ReplayPolicy = ReplayOriginalPolicy
		return nil
	})
}

// RateLimit is the Bits per sec rate limit applied to a push consumer.
func RateLimit(n uint64) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.RateLimit = n
		return nil
	})
}

// BindStream binds a consumer to a stream explicitly based on a name.
func BindStream(name string) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.stream = name
		return nil
	})
}

func (sub *Subscription) ConsumerInfo() (*ConsumerInfo, error) {
	sub.mu.Lock()
	// TODO(dlc) - Better way to mark especially if we attach.
	if sub.jsi.consumer == _EMPTY_ {
		sub.mu.Unlock()
		return nil, ErrTypeSubscription
	}

	// Consumer info lookup should fail if in direct mode.
	js := sub.jsi.js
	if js.opts.direct {
		sub.mu.Unlock()
		return nil, ErrDirectModeRequired
	}

	stream, consumer := sub.jsi.stream, sub.jsi.consumer
	sub.mu.Unlock()

	return js.getConsumerInfo(stream, consumer)
}

type pullOpts struct {
	ttl time.Duration
	ctx context.Context
}

type PullOpt interface {
	configurePull(opts *pullOpts) error
}

// PullMaxWaiting defines the max inflight pull requests to be delivered more messages.
func PullMaxWaiting(n int) SubOpt {
	return subOptFn(func(opts *subOpts) error {
		opts.cfg.MaxWaiting = n
		return nil
	})
}

var errNoMessages = errors.New("nats: no messages")

// Fetch pulls a batch of messages from a stream for a pull consumer.
func (sub *Subscription) Fetch(batch int, opts ...PullOpt) ([]*Msg, error) {
	if sub == nil {
		return nil, ErrBadSubscription
	}

	var o pullOpts
	for _, opt := range opts {
		if err := opt.configurePull(&o); err != nil {
			return nil, err
		}
	}
	if o.ctx != nil && o.ttl != 0 {
		return nil, ErrContextAndTimeout
	}

	sub.mu.Lock()
	if sub.jsi == nil || sub.typ != PullSubscription {
		sub.mu.Unlock()
		return nil, ErrTypeSubscription
	}

	nc, _ := sub.conn, sub.Subject
	stream, consumer := sub.jsi.stream, sub.jsi.consumer
	js := sub.jsi.js

	ttl := o.ttl
	if ttl == 0 {
		ttl = js.opts.wait
	}
	sub.mu.Unlock()

	// Use the given context or setup a default one for the span
	// of the pull batch request.
	var (
		ctx    = o.ctx
		err    error
		cancel context.CancelFunc
	)
	if o.ctx == nil {
		ctx, cancel = context.WithTimeout(context.Background(), ttl)
		defer cancel()
	}

	// Check if context not done already before making the request.
	select {
	case <-ctx.Done():
		if ctx.Err() == context.Canceled {
			err = ctx.Err()
		} else {
			err = ErrTimeout
		}
	default:
	}
	if err != nil {
		return nil, err
	}

	// Check for empty payload message and process synchronously
	// any status messages.
	checkMsg := func(msg *Msg) error {
		if len(msg.Data) == 0 {
			switch msg.Header.Get(statusHdr) {
			case noResponders:
				return ErrNoResponders
			case noMessages:
				return errNoMessages
			case "400", "408", "409":
				return errors.New(msg.Header.Get(descrHdr))
			}
		}
		return nil
	}

	checkCtxErr := func(err error) error {
		if o.ctx == nil && err == context.DeadlineExceeded {
			return ErrTimeout
		}
		return err
	}

	var (
		gotNoMessages bool
		nr            = &nextRequest{Batch: batch, NoWait: true}
		req, _        = json.Marshal(nr)
		reqNext       = js.apiSubj(fmt.Sprintf(apiRequestNextT, stream, consumer))
		expires       = ttl - 10*time.Millisecond
		msgs          = make([]*Msg, 0)
	)

	// In case of only one message, then can already handle with built-in request functions.
	if batch == 1 {
		resp, err := nc.RequestWithContext(ctx, reqNext, req)
		if err != nil {
			return nil, checkCtxErr(err)
		}

		// In case of a no messages instant error, then fallback
		// into longer version of pull batch request.
		err = checkMsg(resp)
		if err != nil {
			if err == errNoMessages {
				// Use old request style for the retry of the pull request
				// in order to use auto UNSUB 1 to prevent the server
				// from delivering a message when there is no more interest.
				nr.NoWait = false
				nr.Expires = expires
				req, _ = json.Marshal(nr)
				resp, err = nc.oldRequestWithContext(ctx, reqNext, nil, req)
				if err != nil {
					return nil, checkCtxErr(err)
				}

				// This next message, could also be an error
				// (e.g. 408 due to request timeout).
				err = checkMsg(resp)
				if err != nil {
					return nil, err
				}
				return []*Msg{resp}, nil
			} else {
				// Hard error
				return nil, checkCtxErr(err)
			}
		}
		return []*Msg{resp}, nil
	}

	// Setup a request where we will wait for the first response
	// in case of errors, then dispatch the rest of the replies
	// to the channel.
	inbox := NewInbox()

	mch := make(chan *Msg, batch)
	s, err := nc.subscribe(inbox, _EMPTY_, nil, mch, true, nil)
	if err != nil {
		return nil, err
	}

	// Remove interest in the subscription at the end so that the
	// this inbox does not get delivered the results intended
	// for another request.
	defer s.Unsubscribe()

	// Make a publish request to get results of the pull.
	err = nc.publish(reqNext, inbox, nil, req)
	if err != nil {
		s.Unsubscribe()
		return nil, err
	}

	// Try to get the first message or error with NoWait.
	var (
		firstMsg *Msg
		ok       bool
	)
	select {
	case firstMsg, ok = <-mch:
		if !ok {
			err = s.getNextMsgErr()
		} else {
			err = s.processNextMsgDelivered(firstMsg)
			if err == nil {
				err = checkMsg(firstMsg)
			}
		}
	case <-ctx.Done():
		err = checkCtxErr(ctx.Err())
	}

	// If the first error is 'no more messages', then switch into
	// longer form version of the request that waits for messages.
	if err == errNoMessages {
		gotNoMessages = true
	} else if err != nil {
		// We should be getting the response from the server
		// in case we got a poll error, so stop and cleanup.
		s.Unsubscribe()
		return nil, err
	}

	if gotNoMessages {
		// We started with a 404 response right away, so fallback into
		// second request that waits longer for messages to delivered.
		nr.NoWait = false
		nr.Expires = expires
		req, _ = json.Marshal(nr)

		// Since first message was an error we UNSUB (batch+1)
		// since we are counting it as the first message.
		err = s.AutoUnsubscribe(batch + 1)
		if err != nil {
			return nil, err
		}

		// Make another request and wait for the messages...
		err = nc.publish(reqNext, inbox, nil, req)
		if err != nil {
			s.Unsubscribe()
			return nil, err
		}

		// Try to get the first result again or return the error.
		select {
		case firstMsg, ok = <-mch:
			if !ok {
				err = s.getNextMsgErr()
			} else {
				err = s.processNextMsgDelivered(firstMsg)
				if err == nil {
					err = checkMsg(firstMsg)
				}
			}
		case <-ctx.Done():
			err = checkCtxErr(ctx.Err())
		}
		if err != nil {
			s.Unsubscribe()
			return nil, err
		}
		// Check again if the delivered next message is a status error.
		err = checkMsg(firstMsg)
		if err != nil {
			s.Unsubscribe()
			return nil, err
		}
	} else {
		// We are receiving messages at this point. Send UNSUB to let
		// the server clear interest once enough replies are delivered.
		err = s.AutoUnsubscribe(batch)
		if err != nil {
			return nil, err
		}
	}

	msgs = append(msgs, firstMsg)
	for {
		var (
			msg *Msg
			ok  bool
		)
		select {
		case msg, ok = <-mch:
			if !ok {
				err = s.getNextMsgErr()
			} else {
				err = s.processNextMsgDelivered(msg)
				if err == nil {
					err = checkMsg(msg)
				}
			}
		case <-ctx.Done():
			return msgs, checkCtxErr(err)
		}
		if err != nil {
			// Discard the error which may have been a timeout
			// or 408 request timeout status from the server,
			// and just the return delivered messages.
			break
		}
		if msg != nil {
			msgs = append(msgs, msg)
		}

		if len(msgs) == batch {
			// Done!
			break
		}
	}

	return msgs, nil
}

func (js *js) getConsumerInfo(stream, consumer string) (*ConsumerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), js.opts.wait)
	defer cancel()
	return js.getConsumerInfoContext(ctx, stream, consumer)
}

func (js *js) getConsumerInfoContext(ctx context.Context, stream, consumer string) (*ConsumerInfo, error) {
	ccInfoSubj := fmt.Sprintf(apiConsumerInfoT, stream, consumer)
	resp, err := js.nc.RequestWithContext(ctx, js.apiSubj(ccInfoSubj), nil)
	if err != nil {
		if err == ErrNoResponders {
			err = ErrJetStreamNotEnabled
		}
		return nil, err
	}

	var info consumerResponse
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		return nil, err
	}
	if info.Error != nil {
		return nil, errors.New(info.Error.Description)
	}
	return info.ConsumerInfo, nil
}

func (m *Msg) checkReply() (*js, bool, error) {
	if m == nil || m.Sub == nil {
		return nil, false, ErrMsgNotBound
	}
	if m.Reply == "" {
		return nil, false, ErrMsgNoReply
	}
	sub := m.Sub
	sub.mu.Lock()
	if sub.jsi == nil {
		sub.mu.Unlock()

		// Not using a JS context.
		return nil, false, nil
	}
	js := sub.jsi.js
	isPullMode := sub.jsi.pull
	sub.mu.Unlock()

	return js, isPullMode, nil
}

// ackReply handles all acks. Will do the right thing for pull and sync mode.
// It ensures that an ack is only sent a single time, regardless of
// how many times it is being called to avoid duplicated acks.
func (m *Msg) ackReply(ackType []byte, sync bool, opts ...PubOpt) error {
	var o pubOpts
	for _, opt := range opts {
		if err := opt.configurePublish(&o); err != nil {
			return err
		}
	}
	js, _, err := m.checkReply()
	if err != nil {
		return err
	}

	// Skip if already acked.
	if atomic.LoadUint32(&m.ackd) == 1 {
		return ErrInvalidJSAck
	}

	m.Sub.mu.Lock()
	nc := m.Sub.conn
	m.Sub.mu.Unlock()

	ctx := o.ctx
	wait := defaultRequestWait
	if o.ttl > 0 {
		wait = o.ttl
	} else if js != nil {
		wait = js.opts.wait
	}

	if sync {
		if ctx != nil {
			_, err = nc.RequestWithContext(ctx, m.Reply, ackType)
		} else {
			_, err = nc.Request(m.Reply, ackType, wait)
		}
	} else {
		err = nc.Publish(m.Reply, ackType)
	}

	// Mark that the message has been acked unless it is AckProgress
	// which can be sent many times.
	if err == nil && !bytes.Equal(ackType, AckProgress) {
		atomic.StoreUint32(&m.ackd, 1)
	}

	return err
}

// Acks for messages

// Ack a message, this will do the right thing with pull based consumers.
func (m *Msg) Ack() error {
	return m.ackReply(AckAck, false)
}

// Ack a message and wait for a response from the server.
func (m *Msg) AckSync(opts ...PubOpt) error {
	return m.ackReply(AckAck, true, opts...)
}

// Nak this message, indicating we can not process.
func (m *Msg) Nak() error {
	return m.ackReply(AckNak, false)
}

// Term this message from ever being delivered regardless of MaxDeliverCount.
func (m *Msg) Term() error {
	return m.ackReply(AckTerm, false)
}

// InProgress indicates that this message is being worked on
// and reset the redelivery timer in the server.
func (m *Msg) InProgress() error {
	return m.ackReply(AckProgress, false)
}

// MsgMetadata is the JetStream metadata associated with received messages.
type MsgMetaData struct {
	Consumer   uint64
	Stream     uint64
	Delivered  uint64
	Pending    uint64
	Timestamp  time.Time
	StreamName string
}

// MetaData retrieves the metadata from a JetStream message.
func (m *Msg) MetaData() (*MsgMetaData, error) {
	if _, _, err := m.checkReply(); err != nil {
		return nil, err
	}

	const expectedTokens = 9
	const btsep = '.'

	tsa := [expectedTokens]string{}
	start, tokens := 0, tsa[:0]
	subject := m.Reply
	for i := 0; i < len(subject); i++ {
		if subject[i] == btsep {
			tokens = append(tokens, subject[start:i])
			start = i + 1
		}
	}
	tokens = append(tokens, subject[start:])
	if len(tokens) != expectedTokens || tokens[0] != "$JS" || tokens[1] != "ACK" {
		return nil, ErrNotJSMessage
	}

	meta := &MsgMetaData{
		Delivered:  uint64(parseNum(tokens[4])),
		Stream:     uint64(parseNum(tokens[5])),
		Consumer:   uint64(parseNum(tokens[6])),
		Timestamp:  time.Unix(0, parseNum(tokens[7])),
		Pending:    uint64(parseNum(tokens[8])),
		StreamName: tokens[2],
	}

	return meta, nil
}

// Quick parser for positive numbers in ack reply encoding.
func parseNum(d string) (n int64) {
	if len(d) == 0 {
		return -1
	}

	// Ascii numbers 0-9
	const (
		asciiZero = 48
		asciiNine = 57
	)

	for _, dec := range d {
		if dec < asciiZero || dec > asciiNine {
			return -1
		}
		n = n*10 + (int64(dec) - asciiZero)
	}
	return n
}

// AckPolicy determines how the consumer should acknowledge delivered messages.
type AckPolicy int

const (
	// AckNonePolicy requires no acks for delivered messages.
	AckNonePolicy AckPolicy = iota

	// AckAllPolicy when acking a sequence number, this implicitly acks all sequences below this one as well.
	AckAllPolicy

	// AckExplicit requires ack or nack for all messages.
	AckExplicitPolicy

	// For setting
	ackPolicyNotSet = 99
)

func jsonString(s string) string {
	return "\"" + s + "\""
}

func (p *AckPolicy) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case jsonString("none"):
		*p = AckNonePolicy
	case jsonString("all"):
		*p = AckAllPolicy
	case jsonString("explicit"):
		*p = AckExplicitPolicy
	default:
		return fmt.Errorf("can not unmarshal %q", data)
	}

	return nil
}

func (p AckPolicy) MarshalJSON() ([]byte, error) {
	switch p {
	case AckNonePolicy:
		return json.Marshal("none")
	case AckAllPolicy:
		return json.Marshal("all")
	case AckExplicitPolicy:
		return json.Marshal("explicit")
	default:
		return nil, fmt.Errorf("unknown acknowlegement policy %v", p)
	}
}

func (p AckPolicy) String() string {
	switch p {
	case AckNonePolicy:
		return "AckNone"
	case AckAllPolicy:
		return "AckAll"
	case AckExplicitPolicy:
		return "AckExplicit"
	case ackPolicyNotSet:
		return "Not Initialized"
	default:
		return "Unknown AckPolicy"
	}
}

// ReplayPolicy determines how the consumer should replay messages it already has queued in the stream.
type ReplayPolicy int

const (
	// ReplayInstant will replay messages as fast as possible.
	ReplayInstantPolicy ReplayPolicy = iota

	// ReplayOriginalPolicy will maintain the same timing as the messages were received.
	ReplayOriginalPolicy
)

func (p *ReplayPolicy) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case jsonString("instant"):
		*p = ReplayInstantPolicy
	case jsonString("original"):
		*p = ReplayOriginalPolicy
	default:
		return fmt.Errorf("can not unmarshal %q", data)
	}

	return nil
}

func (p ReplayPolicy) MarshalJSON() ([]byte, error) {
	switch p {
	case ReplayOriginalPolicy:
		return json.Marshal("original")
	case ReplayInstantPolicy:
		return json.Marshal("instant")
	default:
		return nil, fmt.Errorf("unknown replay policy %v", p)
	}
}

var (
	AckAck      = []byte("+ACK")
	AckNak      = []byte("-NAK")
	AckProgress = []byte("+WPI")
	AckNext     = []byte("+NXT")
	AckTerm     = []byte("+TERM")
)

// DeliverPolicy determines how the consumer should select the first message to deliver.
type DeliverPolicy int

const (
	// DeliverAllPolicy will be the default so can be omitted from the request.
	DeliverAllPolicy DeliverPolicy = iota

	// DeliverLastPolicy will start the consumer with the last sequence received.
	DeliverLastPolicy

	// DeliverNewPolicy will only deliver new messages that are sent
	// after the consumer is created.
	DeliverNewPolicy

	// DeliverByStartSequencePolicy will look for a defined starting sequence to start.
	DeliverByStartSequencePolicy

	// StartTime will select the first messsage with a timestamp >= to StartTime.
	DeliverByStartTimePolicy
)

func (p *DeliverPolicy) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case jsonString("all"), jsonString("undefined"):
		*p = DeliverAllPolicy
	case jsonString("last"):
		*p = DeliverLastPolicy
	case jsonString("new"):
		*p = DeliverNewPolicy
	case jsonString("by_start_sequence"):
		*p = DeliverByStartSequencePolicy
	case jsonString("by_start_time"):
		*p = DeliverByStartTimePolicy
	}

	return nil
}

func (p DeliverPolicy) MarshalJSON() ([]byte, error) {
	switch p {
	case DeliverAllPolicy:
		return json.Marshal("all")
	case DeliverLastPolicy:
		return json.Marshal("last")
	case DeliverNewPolicy:
		return json.Marshal("new")
	case DeliverByStartSequencePolicy:
		return json.Marshal("by_start_sequence")
	case DeliverByStartTimePolicy:
		return json.Marshal("by_start_time")
	default:
		return nil, fmt.Errorf("unknown deliver policy %v", p)
	}
}

// RetentionPolicy determines how messages in a set are retained.
type RetentionPolicy int

const (
	// LimitsPolicy (default) means that messages are retained until any given limit is reached.
	// This could be one of MaxMsgs, MaxBytes, or MaxAge.
	LimitsPolicy RetentionPolicy = iota
	// InterestPolicy specifies that when all known observables have acknowledged a message it can be removed.
	InterestPolicy
	// WorkQueuePolicy specifies that when the first worker or subscriber acknowledges the message it can be removed.
	WorkQueuePolicy
)

// DiscardPolicy determines how we proceed when limits of messages or bytes are hit. The default, DiscardOld will
// remove older messages. DiscardNew will fail to store the new message.
type DiscardPolicy int

const (
	// DiscardOld will remove older messages to return to the limits.
	DiscardOld = iota
	//DiscardNew will error on a StoreMsg call
	DiscardNew
)

const (
	limitsPolicyString    = "limits"
	interestPolicyString  = "interest"
	workQueuePolicyString = "workqueue"
)

func (rp RetentionPolicy) String() string {
	switch rp {
	case LimitsPolicy:
		return "Limits"
	case InterestPolicy:
		return "Interest"
	case WorkQueuePolicy:
		return "WorkQueue"
	default:
		return "Unknown Retention Policy"
	}
}

func (rp RetentionPolicy) MarshalJSON() ([]byte, error) {
	switch rp {
	case LimitsPolicy:
		return json.Marshal(limitsPolicyString)
	case InterestPolicy:
		return json.Marshal(interestPolicyString)
	case WorkQueuePolicy:
		return json.Marshal(workQueuePolicyString)
	default:
		return nil, fmt.Errorf("can not marshal %v", rp)
	}
}

func (rp *RetentionPolicy) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case jsonString(limitsPolicyString):
		*rp = LimitsPolicy
	case jsonString(interestPolicyString):
		*rp = InterestPolicy
	case jsonString(workQueuePolicyString):
		*rp = WorkQueuePolicy
	default:
		return fmt.Errorf("can not unmarshal %q", data)
	}
	return nil
}

func (dp DiscardPolicy) String() string {
	switch dp {
	case DiscardOld:
		return "DiscardOld"
	case DiscardNew:
		return "DiscardNew"
	default:
		return "Unknown Discard Policy"
	}
}

func (dp DiscardPolicy) MarshalJSON() ([]byte, error) {
	switch dp {
	case DiscardOld:
		return json.Marshal("old")
	case DiscardNew:
		return json.Marshal("new")
	default:
		return nil, fmt.Errorf("can not marshal %v", dp)
	}
}

func (dp *DiscardPolicy) UnmarshalJSON(data []byte) error {
	switch strings.ToLower(string(data)) {
	case jsonString("old"):
		*dp = DiscardOld
	case jsonString("new"):
		*dp = DiscardNew
	default:
		return fmt.Errorf("can not unmarshal %q", data)
	}
	return nil
}

// StorageType determines how messages are stored for retention.
type StorageType int

const (
	// FileStorage specifies on disk storage. It's the default.
	FileStorage StorageType = iota
	// MemoryStorage specifies in memory only.
	MemoryStorage
)

const (
	memoryStorageString = "memory"
	fileStorageString   = "file"
)

func (st StorageType) String() string {
	switch st {
	case MemoryStorage:
		return strings.Title(memoryStorageString)
	case FileStorage:
		return strings.Title(fileStorageString)
	default:
		return "Unknown Storage Type"
	}
}

func (st StorageType) MarshalJSON() ([]byte, error) {
	switch st {
	case MemoryStorage:
		return json.Marshal(memoryStorageString)
	case FileStorage:
		return json.Marshal(fileStorageString)
	default:
		return nil, fmt.Errorf("can not marshal %v", st)
	}
}

func (st *StorageType) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case jsonString(memoryStorageString):
		*st = MemoryStorage
	case jsonString(fileStorageString):
		*st = FileStorage
	default:
		return fmt.Errorf("can not unmarshal %q", data)
	}
	return nil
}