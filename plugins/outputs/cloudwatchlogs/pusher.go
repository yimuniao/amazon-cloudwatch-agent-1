// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package cloudwatchlogs

import (
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/amazon-cloudwatch-agent/logs"
	"github.com/aws/amazon-cloudwatch-agent/profiler"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/influxdata/telegraf"
)

const (
	reqSizeLimit   = 1024 * 1024
	reqEventsLimit = 10000
)

var (
	seededRand = rand.New(rand.NewSource(time.Now().UnixNano()))
)

type CloudWatchLogsService interface {
	PutLogEvents(*cloudwatchlogs.PutLogEventsInput) (*cloudwatchlogs.PutLogEventsOutput, error)
	CreateLogStream(input *cloudwatchlogs.CreateLogStreamInput) (*cloudwatchlogs.CreateLogStreamOutput, error)
	CreateLogGroup(input *cloudwatchlogs.CreateLogGroupInput) (*cloudwatchlogs.CreateLogGroupOutput, error)
}

type pusher struct {
	Target
	Service       CloudWatchLogsService
	FlushTimeout  time.Duration
	RetryDuration time.Duration
	Log           telegraf.Logger

	events              []*cloudwatchlogs.InputLogEvent
	minT, maxT          *time.Time
	doneCallbacks       []func()
	eventsCh            chan logs.LogEvent
	nonBlockingEventsCh chan logs.LogEvent
	bufferredSize       int
	flushTimer          *time.Timer
	sequenceToken       *string
	lastValidTime       int64
	needSort            bool
	stop                chan struct{}
	lastSentTime        time.Time

	initNonBlockingChOnce sync.Once
	startNonBlockCh       chan struct{}

	// trace the last time of using pusher
	lastUsedTime time.Time
	// activeCounter indicates the number of incoming metric are currently holding this pusher
	activeCounter    int64
	pusherLock       *sync.Mutex
	multiLogsEnabled bool
}

type svcMockT struct {
	ple func(*cloudwatchlogs.PutLogEventsInput) (*cloudwatchlogs.PutLogEventsOutput, error)
	clg func(input *cloudwatchlogs.CreateLogGroupInput) (*cloudwatchlogs.CreateLogGroupOutput, error)
	cls func(input *cloudwatchlogs.CreateLogStreamInput) (*cloudwatchlogs.CreateLogStreamOutput, error)
}

func (s *svcMockT) PutLogEvents(in *cloudwatchlogs.PutLogEventsInput) (*cloudwatchlogs.PutLogEventsOutput, error) {
	_, err := os.Stat("/tmp/test_flag")
	if err == nil {
		return &cloudwatchlogs.PutLogEventsOutput{NextSequenceToken: nil}, nil

	}
	return &cloudwatchlogs.PutLogEventsOutput{NextSequenceToken: nil}, &cloudwatchlogs.LimitExceededException{}
}
func (s *svcMockT) CreateLogGroup(in *cloudwatchlogs.CreateLogGroupInput) (*cloudwatchlogs.CreateLogGroupOutput, error) {
	if s.clg != nil {
		return s.clg(in)
	}
	return nil, nil
}
func (s *svcMockT) CreateLogStream(in *cloudwatchlogs.CreateLogStreamInput) (*cloudwatchlogs.CreateLogStreamOutput, error) {
	if s.cls != nil {
		return s.cls(in)
	}
	return nil, nil
}

func NewPusher(target Target, service CloudWatchLogsService, flushTimeout time.Duration, retryDuration time.Duration, logger telegraf.Logger, pusherLock *sync.Mutex, multiLogsEnabled bool) *pusher {
	p := &pusher{
		Target:        target,
		Service:       service,
		FlushTimeout:  flushTimeout,
		RetryDuration: retryDuration,
		Log:           logger,

		events:           make([]*cloudwatchlogs.InputLogEvent, 0, 10),
		eventsCh:         make(chan logs.LogEvent, 100),
		flushTimer:       time.NewTimer(flushTimeout),
		stop:             make(chan struct{}),
		startNonBlockCh:  make(chan struct{}),
		pusherLock:       pusherLock,
		multiLogsEnabled: multiLogsEnabled,
	}
	go p.start()
	return p
}

func (p *pusher) GetLastUsedTime() time.Time {
	return p.lastUsedTime
}

func (p *pusher) SetLastUsedTime(time time.Time) {
	p.lastUsedTime = time
}

func (p *pusher) GetActiveCounter() int64 {
	return atomic.LoadInt64(&p.activeCounter)
}

func (p *pusher) IncrementActiveCounter() {
	atomic.AddInt64(&p.activeCounter, 1)
}

func (p *pusher) DecrementActiveCounter(publishCounter int64) {
	atomic.AddInt64(&p.activeCounter, -publishCounter)
}

func (p *pusher) MultiLogsEnabled() bool {
	return p.multiLogsEnabled
}

func (p *pusher) AddEvent(e logs.LogEvent) {
	if !hasValidTime(e) {
		p.Log.Errorf("The log entry in (%v/%v) with timestamp (%v) comparing to the current time (%v) is out of accepted time range. Discard the log entry.", p.Group, p.Stream, e.Time(), time.Now())
		return
	}
	p.eventsCh <- e
}

func (p *pusher) AddEventNonBlocking(e logs.LogEvent) {
	if !hasValidTime(e) {
		p.Log.Errorf("The log entry in (%v/%v) with timestamp (%v) comparing to the current time (%v) is out of accepted time range. Discard the log entry.", p.Group, p.Stream, e.Time(), time.Now())
		return
	}

	p.initNonBlockingChOnce.Do(func() {
		p.nonBlockingEventsCh = make(chan logs.LogEvent, reqEventsLimit*2)
		p.startNonBlockCh <- struct{}{} // Unblock the select loop to recogonize the channel merge
	})

	// Drain the channel until new event can be added
	for {
		select {
		case p.nonBlockingEventsCh <- e:
			return
		default:
			<-p.nonBlockingEventsCh
			p.addStats("emfMetricDrop", 1)
		}
	}
}

func hasValidTime(e logs.LogEvent) bool {
	//http://docs.aws.amazon.com/goto/SdkForGoV1/logs-2014-03-28/PutLogEvents
	//* None of the log events in the batch can be more than 2 hours in the future.
	//* None of the log events in the batch can be older than 14 days or the retention period of the log group.
	if !e.Time().IsZero() {
		now := time.Now()
		dt := now.Sub(e.Time()).Hours()
		if dt > 24*14 || dt < -2 {
			return false
		}
	}
	return true
}

func (p *pusher) Stop() {
	p.flushTimer.Stop()
	close(p.stop)
}

func (p *pusher) start() {
	ec := make(chan logs.LogEvent)

	// Merge events from both blocking and non-blocking channel
	go func() {
		for {
			select {
			case e := <-p.eventsCh:
				p.IncrementActiveCounter()
				ec <- e
			case e := <-p.nonBlockingEventsCh:
				p.IncrementActiveCounter()
				ec <- e
			case <-p.startNonBlockCh:
			case <-p.stop:
				return
			}
		}
	}()

	for {
		select {
		case e := <-ec:
			// Start timer when first event of the batch is added (happens after a flush timer timeout)
			if len(p.events) == 0 {
				p.resetFlushTimer()
			}

			ce := p.convertEvent(e)
			et := time.Unix(*ce.Timestamp/1000, *ce.Timestamp%1000) // Cloudwatch Log Timestamp is in Millisecond

			// A batch of log events in a single request cannot span more than 24 hours.
			if (p.minT != nil && et.Sub(*p.minT) > 24*time.Hour) || (p.maxT != nil && p.maxT.Sub(et) > 24*time.Hour) {
				p.send()
			}

			size := len(*ce.Message) + eventHeaderSize
			if p.bufferredSize+size > reqSizeLimit || len(p.events) == reqEventsLimit {
				p.send()
			}

			if len(p.events) > 0 && *ce.Timestamp < *p.events[len(p.events)-1].Timestamp {
				p.needSort = true
			}

			p.events = append(p.events, ce)
			p.doneCallbacks = append(p.doneCallbacks, e.Done)
			p.bufferredSize += size
			if p.minT == nil || p.minT.After(et) {
				p.minT = &et
			}
			if p.maxT == nil || p.maxT.Before(et) {
				p.maxT = &et
			}

		case <-p.flushTimer.C:
			if time.Since(p.lastSentTime) >= p.FlushTimeout && len(p.events) > 0 {
				p.send()
			} else {
				p.resetFlushTimer()
			}
		case <-p.stop:
			if len(p.events) > 0 {
				p.send()
			}
			return
		}
	}
}

func (p *pusher) reset() {
	for i := 0; i < len(p.events); i++ {
		p.events[i] = nil
	}
	p.events = p.events[:0]
	for i := 0; i < len(p.doneCallbacks); i++ {
		p.doneCallbacks[i] = nil
	}
	p.doneCallbacks = p.doneCallbacks[:0]
	p.bufferredSize = 0
	p.needSort = false
	p.minT = nil
	p.maxT = nil
}

func (p *pusher) send() {
	defer p.resetFlushTimer() // Reset the flush timer after sending the request
	if p.needSort {
		sort.Stable(ByTimestamp(p.events))
	}

	input := &cloudwatchlogs.PutLogEventsInput{
		LogEvents:     p.events,
		LogGroupName:  &p.Group,
		LogStreamName: &p.Stream,
		SequenceToken: p.sequenceToken,
	}

	// set the status when it complets the sending.
	// TODO: If backend is down, pusher for container log will not be deleted.
	// we may need to add protection logic to prevent the memory increasing when backend is down for container log case.
	defer func() {
		p.DecrementActiveCounter(int64(len(input.LogEvents)))
		p.pusherLock.Lock()
		defer p.pusherLock.Unlock()
		p.SetLastUsedTime(time.Now())
		if p.GetActiveCounter() < 0 {
			p.Log.Info("[CounterError] active counter is negative %d", p.GetActiveCounter())
		}
	}()

	startTime := time.Now()

	retryCount := 0
	for {
		input.SequenceToken = p.sequenceToken
		output, err := p.Service.PutLogEvents(input)
		if err == nil {
			if output.NextSequenceToken != nil {
				p.sequenceToken = output.NextSequenceToken
			}
			if output.RejectedLogEventsInfo != nil {
				info := output.RejectedLogEventsInfo
				if info.TooOldLogEventEndIndex != nil {
					p.Log.Warnf("%d log events for log '%s/%s' are too old", *info.TooOldLogEventEndIndex, p.Group, p.Stream)
				}
				if info.TooNewLogEventStartIndex != nil {
					p.Log.Warnf("%d log events for log '%s/%s' are too new", *info.TooNewLogEventStartIndex, p.Group, p.Stream)
				}
				if info.ExpiredLogEventEndIndex != nil {
					p.Log.Warnf("%d log events for log '%s/%s' are expired", *info.ExpiredLogEventEndIndex, p.Group, p.Stream)
				}
			}

			for i := len(p.doneCallbacks) - 1; i >= 0; i-- {
				done := p.doneCallbacks[i]
				done()
			}

			p.Log.Debugf("Pusher published %v log events to group: %v stream: %v with size %v KB in %v.", len(p.events), p.Group, p.Stream, p.bufferredSize/1024, time.Since(startTime))
			p.addStats("rawSize", float64(p.bufferredSize))

			p.reset()
			p.lastSentTime = time.Now()

			return
		}

		awsErr, ok := err.(awserr.Error)
		if !ok {
			p.Log.Errorf("Non aws error received when sending logs to %v/%v: %v", p.Group, p.Stream, err)
			// Messages will be discarded but done callbacks not called
			p.reset()
			return
		}

		switch e := awsErr.(type) {
		case *cloudwatchlogs.ResourceNotFoundException:
			err := p.createLogGroupAndStream()
			if err != nil {
				p.Log.Errorf("Unable to create log stream %v/%v: %v", p.Group, p.Stream, e.Message())
			}
		case *cloudwatchlogs.InvalidSequenceTokenException:
			p.Log.Warnf("Invalid SequenceToken used, will use new token and retry: %v", e.Message())
			if e.ExpectedSequenceToken == nil {
				p.Log.Errorf("Failed to find sequence token from aws response while sending logs to %v/%v: %v", p.Group, p.Stream, e.Message())
			}
			p.sequenceToken = e.ExpectedSequenceToken
		case *cloudwatchlogs.InvalidParameterException,
			*cloudwatchlogs.DataAlreadyAcceptedException:
			p.Log.Errorf("%v, will not retry the request", e)
			p.reset()
			return
		default:
			p.Log.Errorf("Aws error received when sending logs to %v/%v: %v", p.Group, p.Stream, awsErr)
		}

		wait := retryWait(retryCount)
		if time.Since(startTime)+wait > p.RetryDuration {
			p.Log.Errorf("All %v retries to %v/%v failed for PutLogEvents, request dropped.", p.Group, p.Stream, retryCount)
		}

		p.Log.Warnf("Retried %v time, going to sleep %v before retrying.", retryCount, wait)
		time.Sleep(wait)
		retryCount++
	}

}

func retryWait(n int) time.Duration {
	const base = 200 * time.Millisecond
	const max = 1 * time.Minute
	d := base * time.Duration(1<<int64(n))
	if n > 5 {
		d = max
	}
	return time.Duration(seededRand.Int63n(int64(d/2)) + int64(d/2))
}

func (p *pusher) createLogGroupAndStream() error {
	_, err := p.Service.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  &p.Group,
		LogStreamName: &p.Stream,
	})

	if err != nil {
		p.Log.Debugf("creating stream fail due to : %v \n", err)
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == cloudwatchlogs.ErrCodeResourceNotFoundException {
			_, err = p.Service.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
				LogGroupName: &p.Group,
			})

			// create stream again if group created successfully.
			if err == nil {
				_, err = p.Service.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
					LogGroupName:  &p.Group,
					LogStreamName: &p.Stream,
				})
			} else {
				p.Log.Errorf("creating group fail due to : %v \n", err)
			}
		}
	}

	return err
}

func (p *pusher) resetFlushTimer() {
	p.flushTimer.Stop()
	p.flushTimer.Reset(p.FlushTimeout)
}

func (p *pusher) convertEvent(e logs.LogEvent) *cloudwatchlogs.InputLogEvent {
	message := e.Message()

	if len(message) > msgSizeLimit {
		message = message[:msgSizeLimit-len(truncatedSuffix)] + truncatedSuffix
	}
	var t int64
	if e.Time().IsZero() {
		if p.lastValidTime != 0 {
			// Where there has been a valid time before, assume most log events would have
			// a valid timestamp and use the last valid timestamp for new entries that does
			// not have a timestamp.
			t = p.lastValidTime
		} else {
			t = time.Now().UnixNano() / 1000000
		}
	} else {
		t = e.Time().UnixNano() / 1000000
		p.lastValidTime = t
	}
	return &cloudwatchlogs.InputLogEvent{
		Message:   &message,
		Timestamp: &t,
	}
}

func (p *pusher) addStats(statsName string, value float64) {
	statsKey := []string{"cloudwatchlogs", p.Group, statsName}
	profiler.Profiler.AddStats(statsKey, value)
}

type ByTimestamp []*cloudwatchlogs.InputLogEvent

func (inputLogEvents ByTimestamp) Len() int {
	return len(inputLogEvents)
}

func (inputLogEvents ByTimestamp) Swap(i, j int) {
	inputLogEvents[i], inputLogEvents[j] = inputLogEvents[j], inputLogEvents[i]
}

func (inputLogEvents ByTimestamp) Less(i, j int) bool {
	return *inputLogEvents[i].Timestamp < *inputLogEvents[j].Timestamp
}
