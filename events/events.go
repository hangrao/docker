package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/engine"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/parsers/filters"
)

const eventsLimit = 64

type listener chan<- *jsonmessage.JSONMessage

type Events struct {
	mu          sync.RWMutex
	events      []*jsonmessage.JSONMessage
	subscribers []listener
}

func New() *Events {
	return &Events{
		events: make([]*jsonmessage.JSONMessage, 0, eventsLimit),
	}
}

// Install installs events public api in docker engine
func (e *Events) Install(eng *engine.Engine) error {
	// Here you should describe public interface
	jobs := map[string]engine.Handler{
		"events":            e.Get,
		"log":               e.Log,
		"subscribers_count": e.SubscribersCount,
	}
	for name, job := range jobs {
		if err := eng.Register(name, job); err != nil {
			return err
		}
	}
	return nil
}

func (e *Events) Get(job *engine.Job) error {
	var (
		since   = job.GetenvInt64("since")
		until   = job.GetenvInt64("until")
		timeout = time.NewTimer(time.Unix(until, 0).Sub(time.Now()))
	)

	eventFilters, err := filters.FromParam(job.Getenv("filters"))
	if err != nil {
		return err
	}

	// If no until, disable timeout
	if until == 0 {
		timeout.Stop()
	}

	listener := make(chan *jsonmessage.JSONMessage)
	e.subscribe(listener)
	defer e.unsubscribe(listener)

	job.Stdout.Write(nil)

	// Resend every event in the [since, until] time interval.
	if since != 0 {
		if err := e.writeCurrent(job, since, until, eventFilters); err != nil {
			return err
		}
	}

	for {
		select {
		case event, ok := <-listener:
			if !ok {
				return nil
			}
			if err := writeEvent(job, event, eventFilters); err != nil {
				return err
			}
		case <-timeout.C:
			return nil
		}
	}
}

func (e *Events) Log(job *engine.Job) error {
	if len(job.Args) != 3 {
		return fmt.Errorf("usage: %s ACTION ID FROM", job.Name)
	}
	// not waiting for receivers
	go e.log(job.Args[0], job.Args[1], job.Args[2])
	return nil
}

func (e *Events) SubscribersCount(job *engine.Job) error {
	ret := &engine.Env{}
	ret.SetInt("count", e.subscribersCount())
	ret.WriteTo(job.Stdout)
	return nil
}

func writeEvent(job *engine.Job, event *jsonmessage.JSONMessage, eventFilters filters.Args) error {
	isFiltered := func(field string, filter []string) bool {
		if len(filter) == 0 {
			return false
		}
		for _, v := range filter {
			if v == field {
				return false
			}
			if strings.Contains(field, ":") {
				image := strings.Split(field, ":")
				if image[0] == v {
					return false
				}
			}
		}
		return true
	}

	//incoming container filter can be name,id or partial id, convert and replace as a full container id
	for i, cn := range eventFilters["container"] {
		eventFilters["container"][i] = GetContainerId(job.Eng, cn)
	}

	if isFiltered(event.Status, eventFilters["event"]) || isFiltered(event.From, eventFilters["image"]) ||
		isFiltered(event.ID, eventFilters["container"]) {
		return nil
	}

	// When sending an event JSON serialization errors are ignored, but all
	// other errors lead to the eviction of the listener.
	if b, err := json.Marshal(event); err == nil {
		if _, err = job.Stdout.Write(b); err != nil {
			return err
		}
	}
	return nil
}

func (e *Events) writeCurrent(job *engine.Job, since, until int64, eventFilters filters.Args) error {
	e.mu.RLock()
	for _, event := range e.events {
		if event.Time >= since && (event.Time <= until || until == 0) {
			if err := writeEvent(job, event, eventFilters); err != nil {
				e.mu.RUnlock()
				return err
			}
		}
	}
	e.mu.RUnlock()
	return nil
}

func (e *Events) subscribersCount() int {
	e.mu.RLock()
	c := len(e.subscribers)
	e.mu.RUnlock()
	return c
}

func (e *Events) log(action, id, from string) {
	e.mu.Lock()
	now := time.Now().UTC().Unix()
	jm := &jsonmessage.JSONMessage{Status: action, ID: id, From: from, Time: now}
	if len(e.events) == cap(e.events) {
		// discard oldest event
		copy(e.events, e.events[1:])
		e.events[len(e.events)-1] = jm
	} else {
		e.events = append(e.events, jm)
	}
	for _, s := range e.subscribers {
		// We give each subscriber a 100ms time window to receive the event,
		// after which we move to the next.
		select {
		case s <- jm:
		case <-time.After(100 * time.Millisecond):
		}
	}
	e.mu.Unlock()
}

func (e *Events) subscribe(l listener) {
	e.mu.Lock()
	e.subscribers = append(e.subscribers, l)
	e.mu.Unlock()
}

// unsubscribe closes and removes the specified listener from the list of
// previously registed ones.
// It returns a boolean value indicating if the listener was successfully
// found, closed and unregistered.
func (e *Events) unsubscribe(l listener) bool {
	e.mu.Lock()
	for i, subscriber := range e.subscribers {
		if subscriber == l {
			close(l)
			e.subscribers = append(e.subscribers[:i], e.subscribers[i+1:]...)
			e.mu.Unlock()
			return true
		}
	}
	e.mu.Unlock()
	return false
}

func GetContainerId(eng *engine.Engine, name string) string {
	var buf bytes.Buffer
	job := eng.Job("container_inspect", name)

	var outStream io.Writer

	outStream = &buf
	job.Stdout.Set(outStream)

	if err := job.Run(); err != nil {
		return ""
	}
	var out struct{ ID string }
	json.NewDecoder(&buf).Decode(&out)
	return out.ID
}
