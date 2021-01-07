// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package cloudwatchlogs

import (
	"fmt"
	"github.com/aws/amazon-cloudwatch-agent/internal"
	"github.com/influxdata/telegraf/plugins/outputs"
	"log"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type TestLogger struct {
	t *testing.T
}

func (tl TestLogger) Errorf(format string, args ...interface{}) {
	tl.t.Errorf(format, args...)
}
func (tl TestLogger) Error(args ...interface{}) {
	tl.t.Error(args...)
}
func (tl TestLogger) Debugf(format string, args ...interface{}) { log.Printf(format, args...) }
func (tl TestLogger) Debug(args ...interface{})                 { log.Println(args...) }
func (tl TestLogger) Warnf(format string, args ...interface{})  { log.Printf(format, args...) }
func (tl TestLogger) Warn(args ...interface{})                  { log.Println(args...) }
func (tl TestLogger) Infof(format string, args ...interface{})  { log.Printf(format, args...) }
func (tl TestLogger) Info(args ...interface{})                  { log.Println(args...) }

func TestCreateDest(t *testing.T) {
	// Test filename as log group name
	c := outputs.Outputs["cloudwatchlogs"]().(*CloudWatchLogs)
	c.Log = &TestLogger{t: t}
	c.LogStreamName = "STREAM"

	d0 := c.CreateDest("GROUP", "OTHER_STREAM", false).(*cwDest)
	if d0.pusher.Group != "GROUP" || d0.pusher.Stream != "OTHER_STREAM" {
		t.Errorf("Wrong target for the created cwDest: %s/%s, expecting GROUP/OTHER_STREAM", d0.pusher.Group, d0.pusher.Stream)
	}

	d1 := c.CreateDest("FILENAME", "", false).(*cwDest)
	if d1.pusher.Group != "FILENAME" || d1.pusher.Stream != "STREAM" {
		t.Errorf("Wrong target for the created cwDest: %s/%s, expecting FILENAME/STREAM", d1.pusher.Group, d1.pusher.Stream)
	}

	d2 := c.CreateDest("FILENAME", "", false).(*cwDest)

	if d1 != d2 {
		t.Errorf("Create dest with the same name should return the same cwDest")
	}

	d3 := c.CreateDest("ANOTHERFILE", "", false).(*cwDest)
	if d1 == d3 {
		t.Errorf("Different file name should result in different cwDest")
	}

	c.LogGroupName = "G1"
	c.LogStreamName = "S1"

	d := c.CreateDest("", "", false).(*cwDest)

	if d.pusher.Group != "G1" || d.pusher.Stream != "S1" {
		t.Errorf("Empty create dest should return dest to default group and stream, %v/%v found", d.pusher.Group, d.pusher.Stream)
	}
}

func TestCloudWatchLogs_scanObsoletePushers(t *testing.T) {
	logs := &CloudWatchLogs{
		cwDests:      map[Target]*cwDest{},
		obsoletePusherQueue:        NewObsoletePusherQueue(50),
		ForceFlushInterval:         internal.Duration{Duration: time.Second},
		shutdownChan:               make(chan bool),
		forcePusherCleanupChan:     make(chan bool),
		Log: &TestLogger{t: t},
	}
	times := [4]time.Time{time.Now().Add(-2 * time.Minute), time.Now(), time.Now(), time.Now().Add(-2 * time.Minute)}
	for i := 0; i < 4; i++ {
		dest := logs.getDest(Target{"test_log_group", fmt.Sprintf("test_log_stream_%d", i)}, true)
		dest.SetLastUsedTime(times[i])
	}
	go logs.scanObsoletePusherPeriodically(10*time.Second, pusherExpiryTime)
	time.Sleep(15 * time.Second)
	close(logs.shutdownChan)
	assert.Equal(t, 2, len(logs.cwDests))

	_, ok := logs.cwDests[Target{"test_log_group","test_log_stream_0"}]
	assert.False(t, ok)

	for i := 1; i < 3; i++ {
		_, ok := logs.cwDests[Target{"test_log_group",fmt.Sprintf("test_log_stream_%d", i)}]
		assert.True(t, ok)
	}

	_, ok = logs.cwDests[Target{"test_log_group","test_log_stream_4"}]
	assert.False(t, ok)
}

func TestCloudWatchLogs_scanObsoletePushersWithMultilogsDisabled(t *testing.T) {
	logs := &CloudWatchLogs{
		cwDests:      map[Target]*cwDest{},
		obsoletePusherQueue:        NewObsoletePusherQueue(50),
		ForceFlushInterval:         internal.Duration{Duration: time.Second},
		shutdownChan:               make(chan bool),
		forcePusherCleanupChan:     make(chan bool),
		Log: &TestLogger{t: t},
	}
	times := [4]time.Time{time.Now().Add(-2 * time.Minute), time.Now(), time.Now(), time.Now().Add(-2 * time.Minute)}
	for i := 0; i < 4; i++ {
		dest := logs.getDest(Target{"test_log_group", fmt.Sprintf("test_log_stream_%d", i)}, false)
		dest.SetLastUsedTime(times[i])
	}
	go logs.scanObsoletePusherPeriodically(10*time.Second, pusherExpiryTime)
	time.Sleep(15 * time.Second)
	close(logs.shutdownChan)
	assert.Equal(t, 4, len(logs.cwDests))
	for i := 0; i < 4; i++ {
		_, ok := logs.cwDests[Target{"test_log_group",fmt.Sprintf("test_log_stream_%d", i)}]
		assert.True(t, ok)
	}
}

func TestCloudWatchLogs_cleanupPushers(t *testing.T) {
	logs := &CloudWatchLogs{
		cwDests:      map[Target]*cwDest{},
		obsoletePusherQueue:        NewObsoletePusherQueue(50),
		ForceFlushInterval:         internal.Duration{Duration: time.Second},
		shutdownChan:               make(chan bool),
		forcePusherCleanupChan:     make(chan bool),
		Log: &TestLogger{t: t},
	}
	for i := 0; i < 10; i++ {
		dest := logs.getDest(Target{"test_log_group", fmt.Sprintf("test_log_stream_%d", i)}, true)
		logs.obsoletePusherQueue.Enqueue(dest)
	}
	go logs.cleanupPushers()
	time.Sleep(5 * time.Second)
	close(logs.shutdownChan)

	_, ok := logs.obsoletePusherQueue.Dequeue()
	assert.False(t, ok)
}

func TestCloudWatchLogs_forceCleanObsoletePushers(t *testing.T) {
	logs := &CloudWatchLogs{
		cwDests:      map[Target]*cwDest{},
		obsoletePusherQueue:        NewObsoletePusherQueue(20),
		ForceFlushInterval:         internal.Duration{Duration: time.Second},
		shutdownChan:               make(chan bool),
		forcePusherCleanupChan:     make(chan bool),
		Log: &TestLogger{t: t},
	}
	go func() {
		for {
			select {
			case <-logs.forcePusherCleanupChan:
				for i := 0; i < 10; i++ {
					dest, ok := logs.obsoletePusherQueue.Dequeue()
					if ok {
						dest.(*cwDest).Stop(true)
					} else {
						break
					}
				}
			case <-logs.shutdownChan:
				return
			}
		}
	}()
	var forceClean bool
	for i := 0; i < 19; i++ {
		dest :=  logs.getDest(Target{"test_log_group", fmt.Sprintf("test_log_stream_%d", i)}, true)
		forceClean = logs.obsoletePusherQueue.Enqueue(dest)
		assert.False(t, forceClean)
	}
	dest := logs.getDest(Target{"test_log_group", fmt.Sprintf("test_log_stream_%d", 19)}, true)
	forceClean = logs.obsoletePusherQueue.Enqueue(dest)
	assert.True(t, forceClean)
	logs.forcePusherCleanupChan <- true
	time.Sleep(5 * time.Second)
	close(logs.shutdownChan)
	var ok bool
	for i := 0; i < 10; i++ {
		_, ok = logs.obsoletePusherQueue.Dequeue()
		assert.True(t, ok)
	}
	_, ok = logs.obsoletePusherQueue.Dequeue()
	assert.False(t, ok)
}

func TestCloudWatchLogs_pusherMapRaceCondition(t *testing.T) {
	logs := &CloudWatchLogs{
		cwDests:      map[Target]*cwDest{},
		obsoletePusherQueue:        NewObsoletePusherQueue(50),
		ForceFlushInterval:         internal.Duration{Duration: time.Second},
		shutdownChan:               make(chan bool),
		forcePusherCleanupChan:     make(chan bool),
		Log: &TestLogger{t: t},
	}
	pusherShutdownChan := make(chan bool)
	pusherExpiryTime := 3 * time.Second
	go MimicGetPushers(pusherShutdownChan, logs)
	go logs.scanObsoletePusherPeriodically(3*time.Second, pusherExpiryTime)
	go ObsoletePusherQueueValidator(t, logs.shutdownChan, logs, pusherExpiryTime)

	time.Sleep(time.Minute)
	close(pusherShutdownChan)
	time.Sleep(2 * time.Minute)
	close(logs.shutdownChan)
	pusherNum := len(logs.cwDests)
	assert.Equal(t, 0, pusherNum)
}

func MimicGetPushers(pusherShutdownChan <-chan bool, logs *CloudWatchLogs) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	rand.Seed(time.Now().UnixNano())
	for {
		select {
		case <-ticker.C:
			randomInt := rand.Intn(20)
			logStream := fmt.Sprintf("test_log_stream_%d", randomInt)
			logGroup := fmt.Sprintf("test_log_group_%d", randomInt%3)

			pusher := logs.getDest(Target{logGroup, logStream}, true)
			pusher.IncrementActiveCounter()

			// mimic the operation after publishing
			go func() {
				time.Sleep(100 * time.Millisecond)
				pusher.SetLastUsedTime(time.Now())
				pusher.DecrementActiveCounter(1)
			}()
		case <-pusherShutdownChan:
			return
		}
	}
}

func ObsoletePusherQueueValidator(t *testing.T, validatorShutdownChan <-chan bool, logs *CloudWatchLogs, pusherExpiryTime time.Duration) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	obsoletePusherQueue := logs.obsoletePusherQueue
	for {
		select {
		case <-ticker.C:
			pusher, ok := obsoletePusherQueue.Dequeue()
			if ok {
				// Do validation. make sure all the pushers coming into cleanup queue are really obsolete
				// (by checking lastUsedTime and activeCounter)
				assert.True(t, pusher.(*cwDest).GetLastUsedTime().Add(pusherExpiryTime).Before(time.Now()))
				assert.True(t, pusher.(*cwDest).GetActiveCounter() == 0)
			}
		case <-validatorShutdownChan:
			return
		}
	}
}
