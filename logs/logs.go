// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package logs

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/influxdata/telegraf/config"
)

var ErrOutputStopped = errors.New("Output plugin stopped")
var ErrOutputCleanedup = errors.New("Output plugin ErrOutputCleanedup")

// A LogCollection is a collection of LogSrc, a plugin which can provide many LogSrc
type LogCollection interface {
	FindLogSrc() []LogSrc
}

type LogEvent interface {
	Message() string
	Time() time.Time
	Done()
}

// A LogSrc is a single source where log events are generated
// e.g. a single log file
type LogSrc interface {
	SetOutput(func(LogEvent))
	Group() string
	Stream() string
	Destination() string
	Description() string
	Stop()
 	PublishMultiLogs() bool
}

// A LogBackend is able to return a LogDest of a given name.
// The same name should always return the same LogDest.
type LogBackend interface {
	CreateDest(string, string, bool) LogDest
}

// A LogDest represents a final endpoint where log events are published to.
// e.g. a particualr log stream in cloudwatchlogs.
type LogDest interface {
	Publish(events []LogEvent) error
}

// LogAgent is the agent handles pure log pipelines
type LogAgent struct {
	Config      *config.Config
	backends    map[string]LogBackend
	collections []LogCollection
}

func NewLogAgent(c *config.Config) *LogAgent {
	return &LogAgent{
		Config:    c,
		backends:  make(map[string]LogBackend),
	}
}

// LogAgent will scan all input and output plugins for LogCollection and LogBackend.
// And connect all the LogSrc from the LogCollection found to the respective LogDest
// based on the configured "destination", and "name"
func (l *LogAgent) Run(ctx context.Context) {
	log.Printf("I! [logagent] starting")
	for _, output := range l.Config.Outputs {
		backend, ok := output.Output.(LogBackend)
		if !ok {
			continue
		}
		log.Printf("I! [logagent] found plugin %v is a log backend", output.Config.Name)
		name := output.Config.Alias
		if name == "" {
			name = output.Config.Name
		}
		l.backends[name] = backend
	}

	for _, input := range l.Config.Inputs {
		if collection, ok := input.Input.(LogCollection); ok {
			log.Printf("I! [logagent] found plugin %v is a log collection", input.Config.Name)
			l.collections = append(l.collections, collection)
		}
	}

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			for _, c := range l.collections {
				srcs := c.FindLogSrc()
				for _, src := range srcs {
					dname := src.Destination()
					backend, ok := l.backends[dname]
					if !ok {
						log.Printf("E! [logagent] Failed to find destination %v for log source %v/%v(%v) ", dname, src.Group(), src.Stream(), src.Description())
						continue
					}
					log.Printf("I! [logagent] piping log from %v/%v(%v) to %v", src.Group(), src.Stream(), src.Description(), dname)
					go l.runSrcToDest(src, dname, backend)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (l *LogAgent) runSrcToDest(src LogSrc, dname string, backend LogBackend) {
	eventsCh := make(chan LogEvent)
	defer src.Stop()

	src.SetOutput(func(e LogEvent) {
		if e == nil {
			close(eventsCh)
			log.Printf("I! [logagent] Log src has stopped for %v/%v(%v)", src.Group(), src.Stream(), src.Description())
			return
		}
		eventsCh <- e
	})

	dest := backend.CreateDest(src.Group(), src.Stream(), src.PublishMultiLogs())
	for e := range eventsCh {
		err := dest.Publish([]LogEvent{e})
		if err == ErrOutputCleanedup {
			log.Printf("I! [logagent] Log destination %v has been cleanedup, finalizing %v/%v", dname, src.Group(), src.Stream())
			dest = backend.CreateDest(src.Group(), src.Stream(), src.PublishMultiLogs())
			err = dest.Publish([]LogEvent{e})
		}

		if err == ErrOutputStopped {
			log.Printf("I! [logagent] Log destination %v has stopped, finalizing %v/%v", dname, src.Group(), src.Stream())
			return
		}

		if err != nil {
			log.Printf("E! [logagent] Failed to publish log to %v, error: %v", dname, err)
			return
		}
	}
}
