// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package cloudwatchlogs

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/amazon-cloudwatch-agent/cfg/agentinfo"
	configaws "github.com/aws/amazon-cloudwatch-agent/cfg/aws"
	"github.com/aws/amazon-cloudwatch-agent/handlers"
	"github.com/aws/amazon-cloudwatch-agent/internal"
	"github.com/aws/amazon-cloudwatch-agent/logs"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/outputs"
)

const (
	LogGroupNameTag   = "log_group_name"
	LogStreamNameTag  = "log_stream_name"
	LogTimestampField = "log_timestamp"
	LogEntryField     = "value"

	defaultFlushTimeout            = 5 * time.Second
	pusherCleanupInterval          = 1 * time.Minute
	pusherExpiryTime               = 1 * time.Minute
	cleanerCleanInterval           = 50 * time.Millisecond
	initialObsoletePusherQueueSize = 100
	finalObsoletePusherQueueSize   = 6400
	eventHeaderSize                = 26
	truncatedSuffix                = "[Truncated...]"
	msgSizeLimit                   = 256*1024 - eventHeaderSize

	maxRetryTimeout    = 14*24*time.Hour + 10*time.Minute
	metricRetryTimeout = 2 * time.Minute

	attributesInFields = "attributesInFields"
)

type CloudWatchLogs struct {
	Region           string `toml:"region"`
	EndpointOverride string `toml:"endpoint_override"`
	AccessKey        string `toml:"access_key"`
	SecretKey        string `toml:"secret_key"`
	RoleARN          string `toml:"role_arn"`
	Profile          string `toml:"profile"`
	Filename         string `toml:"shared_credential_file"`
	Token            string `toml:"token"`

	//log group and stream names
	LogStreamName string `toml:"log_stream_name"`
	LogGroupName  string `toml:"log_group_name"`

	ForceFlushInterval internal.Duration `toml:"force_flush_interval"` // unit is second

	Log telegraf.Logger `toml:"-"`

	cwDests       map[Target]*cwDest
	pusherMapLock sync.RWMutex


	numPusherEverCreated int64

	obsoletePusherQueue    *ObsoletePusherQueue
	forcePusherCleanupChan chan bool

	shutdownChan chan bool
}

func (c *CloudWatchLogs) scanObsoletePusherPeriodically(pusherCleanupInterval time.Duration, pusherExpiryTime time.Duration) {
	ticker := time.NewTicker(pusherCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.scanObsoletePushers(pusherExpiryTime)
		case <-c.shutdownChan:
			return
		}
	}
}

func (c *CloudWatchLogs) scanObsoletePushers(pusherExpiryTime time.Duration) {
	// get the keys first, it can avoid to lock the whole map for long time.
	c.pusherMapLock.RLock()
	log.Printf("D! total cwDests: %v", len(c.cwDests))
	keys := make([]Target, 0, len(c.cwDests))
	for k := range c.cwDests {
		keys = append(keys, k)
	}
	c.pusherMapLock.RUnlock()

	for _, target := range keys {
		// skip if multi-logs feature is not used for this pusher
		c.pusherMapLock.RLock()
		cd, ok := c.cwDests[target]
		c.pusherMapLock.RUnlock()
		if !ok {
			continue
		}

		if !cd.MultiLogsEnabled() {
			continue
		}

		cd.pusher.pusherLock.Lock()
		if cd.GetLastUsedTime().Add(pusherExpiryTime).Before(time.Now()) && cd.GetActiveCounter() == 0 {
			cd.pusher.pusherLock.Unlock()
			// remove from map and Enqueue
			c.pusherMapLock.Lock()
			delete(c.cwDests, target)
			c.pusherMapLock.Unlock()
			forceClean := c.obsoletePusherQueue.Enqueue(cd)
			c.Log.Debug("Delete expired logpusher from map", target)
			if forceClean {
				if c.obsoletePusherQueue.GetCurrentSizeLimit() < finalObsoletePusherQueueSize {
					c.obsoletePusherQueue.DoubleSizeLimit()
				}
				c.forcePusherCleanupChan <- true
			}
		} else {
			cd.pusher.pusherLock.Unlock()
		}

	}
}

func (c *CloudWatchLogs) cleanupPushers() {
	ticker := time.NewTicker(cleanerCleanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			dest, ok := c.obsoletePusherQueue.Dequeue()
			if ok {
				c.Log.Debug("Delete dest:", dest.(*cwDest))
				dest.(*cwDest).Stop(true)
			} else {
				c.Log.Debug("Obsolete pusher queue is drained, wait for 1 minute to start next cleanup.")
				time.Sleep(1 * time.Minute)
			}
		case <-c.forcePusherCleanupChan:
			for i := 0; i < c.obsoletePusherQueue.GetCurrentSizeLimit()/4; i++ {
				dest, ok := c.obsoletePusherQueue.Dequeue()
				if ok {
					dest.(*cwDest).Stop(true)
				} else {
					break
				}
			}
		case <-c.shutdownChan:
			return
		}
	}
}

func (c *CloudWatchLogs) Connect() error {
	go c.scanObsoletePusherPeriodically(pusherCleanupInterval, pusherExpiryTime)
	go c.cleanupPushers()
	return nil
}

func (c *CloudWatchLogs) Close() error {
	c.pusherMapLock.RLock()
	defer c.pusherMapLock.RUnlock()
	for _, d := range c.cwDests {
		d.Stop(false)
	}
	close(c.shutdownChan)
	return nil
}

func (c *CloudWatchLogs) Write(metrics []telegraf.Metric) error {
	for _, m := range metrics {
		c.writeMetricAsStructuredLog(m)
	}
	return nil
}

func (c *CloudWatchLogs) CreateDest(group, stream string, multiLogsEnabled bool) logs.LogDest {
	if group == "" {
		group = c.LogGroupName
	}
	if stream == "" {
		stream = c.LogStreamName
	}

	t := Target{
		Group:  group,
		Stream: stream,
	}
	return c.getDest(t, multiLogsEnabled)
}

func (c *CloudWatchLogs) getDest(t Target, multiLogsEnabled bool) *cwDest {
	c.pusherMapLock.RLock()
	if cwd, ok := c.cwDests[t]; ok {
		c.pusherMapLock.RUnlock()

		cwd.pusherLock.Lock()
		cwd.SetLastUsedTime(time.Now())
		cwd.pusherLock.Unlock()
		return cwd
	}
	c.pusherMapLock.RUnlock()

	credentialConfig := &configaws.CredentialConfig{
		Region:    c.Region,
		AccessKey: c.AccessKey,
		SecretKey: c.SecretKey,
		RoleARN:   c.RoleARN,
		Profile:   c.Profile,
		Filename:  c.Filename,
		Token:     c.Token,
	}

	client := cloudwatchlogs.New(
		credentialConfig.Credentials(),
		&aws.Config{
			Endpoint: aws.String(c.EndpointOverride),
		},
	)
	client.Handlers.Build.PushBackNamed(handlers.NewRequestCompressionHandler([]string{"PutLogEvents"}))
	client.Handlers.Build.PushBackNamed(handlers.NewCustomHeaderHandler("User-Agent", agentinfo.UserAgent()))

    var pusher *pusher
	_, err := os.Stat("/tmp/test_real_backend")
	if err != nil {
		pusher = NewPusher(t, &svcMockT{}, c.ForceFlushInterval.Duration, maxRetryTimeout, c.Log, &sync.Mutex{}, multiLogsEnabled)
	} else {
		pusher = NewPusher(t, client, c.ForceFlushInterval.Duration, maxRetryTimeout, c.Log, &sync.Mutex{}, multiLogsEnabled)
	}

	cwd := &cwDest{pusher: pusher}
	c.pusherMapLock.Lock()
	cwd.SetLastUsedTime(time.Now())
	c.cwDests[t] = cwd
	c.pusherMapLock.Unlock()
	c.numPusherEverCreated += 1
	c.Log.Debug("[pusherNum]Creating new pusher, total number of pusher ever created is %d", c.numPusherEverCreated)

	return cwd
}

func (c *CloudWatchLogs)TotalCachedEvents() int64 {
	c.pusherMapLock.RLock()
	defer c.pusherMapLock.RUnlock()
	totalLength := int64(0)
	for _,v := range c.cwDests {
		totalLength += int64(len(v.eventsCh))
	}

	return totalLength
}

func (c *CloudWatchLogs) writeMetricAsStructuredLog(m telegraf.Metric) {
	t, err := c.getTargetFromMetric(m)
	if err != nil {
		c.Log.Errorf("Failed to find target: ", err)
	}
	cwd := c.getDest(t, false)
	if cwd == nil {
		c.Log.Warnf("unable to find log destination, group:", t.Group, ", stream:",  t.Stream)
		return
	}
	cwd.switchToEMF()
	cwd.pusher.RetryDuration = metricRetryTimeout

	e := c.getLogEventFromMetric(m)
	if e == nil {
		return
	}

	cwd.AddEvent(e, true)
}

func (c *CloudWatchLogs) getTargetFromMetric(m telegraf.Metric) (Target, error) {
	tags := m.Tags()
	logGroup, ok := tags[LogGroupNameTag]
	if !ok {
		return Target{}, fmt.Errorf("structuredlog receive a metric with name '%v' without log group name", m.Name())
	} else {
		m.RemoveTag(LogGroupNameTag)
	}

	logStream, ok := tags[LogStreamNameTag]
	if ok {
		m.RemoveTag(LogStreamNameTag)
	} else if logStream == "" {
		logStream = c.LogStreamName
	}

	return Target{logGroup, logStream}, nil
}

func (c *CloudWatchLogs) getLogEventFromMetric(metric telegraf.Metric) *structuredLogEvent {
	var message string
	if metric.HasField(LogEntryField) {
		var ok bool
		if message, ok = metric.Fields()[LogEntryField].(string); !ok {
			c.Log.Warnf("The log entry value field is not string type: %v", metric.Fields())
			return nil
		}
	} else {
		content := map[string]interface{}{}
		tags := metric.Tags()
		// build all the attributesInFields
		if val, ok := tags[attributesInFields]; ok {
			attributes := strings.Split(val, ",")
			mFields := metric.Fields()
			for _, attr := range attributes {
				if fieldVal, ok := mFields[attr]; ok {
					content[attr] = fieldVal
					metric.RemoveField(attr)
				}
			}
			metric.RemoveTag(attributesInFields)
			delete(tags, attributesInFields)
		}

		// build remaining attributes
		for k := range tags {
			content[k] = tags[k]
		}

		for k, v := range metric.Fields() {
			var value interface{}

			switch t := v.(type) {
			case int:
				value = float64(t)
			case int32:
				value = float64(t)
			case int64:
				value = float64(t)
			case uint:
				value = float64(t)
			case uint32:
				value = float64(t)
			case uint64:
				value = float64(t)
			case float64:
				value = t
			case bool:
				value = t
			case string:
				value = t
			case time.Time:
				value = float64(t.Unix())

			default:
				c.Log.Errorf("Detected unexpected fields (%s,%v) when encoding structured log event, value type %T is not supported", k, v, v)
				return nil
			}
			content[k] = value
		}

		jsonMap, err := json.Marshal(content)
		if err != nil {
			c.Log.Errorf("Unalbe to marshal structured log content: %v", err)
		}
		message = string(jsonMap)
	}

	return &structuredLogEvent{
		msg: message,
		t:   metric.Time(),
	}
}

type structuredLogEvent struct {
	msg string
	t   time.Time
}

func (e *structuredLogEvent) Message() string {
	return e.msg
}

func (e *structuredLogEvent) Time() time.Time {
	return e.t
}

func (e *structuredLogEvent) Done() {}

type cwDest struct {
	*pusher
	sync.RWMutex
	isEMF   bool
	stopped bool
	cleanedup bool
}

func (cd *cwDest) Publish(events []logs.LogEvent) error {
	cd.RLock()
	defer cd.RUnlock()
	if cd.cleanedup {
		return logs.ErrOutputCleanedup
	}
	if cd.stopped {
		return logs.ErrOutputStopped
	}

	for _, e := range events {
		if !cd.isEMF {
			msg := e.Message()
			if strings.HasPrefix(msg, "{") && strings.HasSuffix(msg, "}") && strings.Contains(msg, "\"CloudWatchMetrics\"") {
				cd.switchToEMF()
			}
		}
		cd.AddEvent(e, false)
	}
	return nil
}

func (cd *cwDest) Stop(isCleanup bool) {
	cd.Lock()
	cd.stopped = true
	cd.cleanedup = isCleanup
	cd.Unlock()
	cd.pusher.Stop()
}

func (cd *cwDest) AddEvent(e logs.LogEvent, nonBlocking bool) {

	// Drop events for metric path logs when queue is full
	// Only use non blocking mode if the event comes from metric path
	if nonBlocking {
		cd.pusher.AddEventNonBlocking(e)
	} else {
		cd.pusher.AddEvent(e)
	}
}

func (cd *cwDest) switchToEMF() {
	cd.Lock()
	defer cd.Unlock()
	if !cd.isEMF {
		cd.isEMF = true
		cwl, ok := cd.Service.(*cloudwatchlogs.CloudWatchLogs)
		if ok {
			cwl.Handlers.Build.PushBackNamed(handlers.NewCustomHeaderHandler("x-amzn-logs-format", "json/emf"))
		}
	}
}

func (cd *cwDest) setRetryer(r request.Retryer) {
	cwl, ok := cd.Service.(*cloudwatchlogs.CloudWatchLogs)
	if ok {
		cwl.Retryer = r
	}
}

type Target struct {
	Group, Stream string
}

// Description returns a one-sentence description on the Output
func (c *CloudWatchLogs) Description() string {
	return "Configuration for AWS CloudWatchLogs output."
}

var sampleConfig = `
  ## Amazon REGION
  region = "us-east-1"

  ## Amazon Credentials
  ## Credentials are loaded in the following order
  ## 1) Assumed credentials via STS if role_arn is specified
  ## 2) explicit credentials from 'access_key' and 'secret_key'
  ## 3) shared profile from 'profile'
  ## 4) environment variables
  ## 5) shared credentials file
  ## 6) EC2 Instance Profile
  #access_key = ""
  #secret_key = ""
  #token = ""
  #role_arn = ""
  #profile = ""
  #shared_credential_file = ""

  # The log stream name.
  log_stream_name = "<log_stream_name>"
`

// SampleConfig returns the default configuration of the Output
func (c *CloudWatchLogs) SampleConfig() string {
	return sampleConfig
}

func init() {
	outputs.Add("cloudwatchlogs", func() telegraf.Output {
		return &CloudWatchLogs{
			ForceFlushInterval:         internal.Duration{Duration: defaultFlushTimeout},
			cwDests:                    make(map[Target]*cwDest),
			obsoletePusherQueue:        NewObsoletePusherQueue(initialObsoletePusherQueueSize),
			forcePusherCleanupChan:     make(chan bool),
			shutdownChan:               make(chan bool),
		}
	})
}
