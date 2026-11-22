package mapping

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Port is a readable port of the ALSA sequencer.
type Port struct {
	Client     int    `json:"client"`
	Port       int    `json:"port"`
	ClientName string `json:"clientName"`
	PortName   string `json:"portName"`
}

// Source is a read-only view of the controller's MIDI notes: it subscribes to one port,
// reads, and unsubscribes on Close. Engine's own subscription is never touched, and
// nothing is subscribed while no learn is running.
type Source interface {
	Ports() ([]Port, error)
	Subscribe(Port) error
	Next(wait time.Duration) ([]Event, error)
	Close() error
}

// Errors of a learn run.
var (
	ErrLearnTimeout = errors.New("no gesture within the learn window")
	ErrNoController = errors.New("no controller port found in the ALSA sequencer")
)

// The controller's ALSA client is named after the model, e.g. "SC5000 Prime Controller".
var modelHint = map[string]string{"JP07": "sc5000", "JP08": "sc5000m", "JP13": "sc6000", "JP14": "sc6000m"}

// PickPort chooses the board's controller port among the readable ports.
func PickPort(board string, ports []Port) (Port, error) {
	hint := modelHint[board]
	best, bestScore, ties := Port{}, 0, 0
	for _, p := range ports {
		name := strings.ToLower(p.ClientName)
		if !strings.Contains(name, "controller") || strings.HasPrefix(name, "enginex") || strings.HasPrefix(name, "midi through") {
			continue
		}
		score := 1
		if hint != "" && strings.HasPrefix(name, hint+" ") {
			score = 2
		}
		switch {
		case score > bestScore:
			best, bestScore, ties = p, score, 1
		case score == bestScore:
			ties++
		}
	}
	switch {
	case bestScore == 0:
		return Port{}, ErrNoController
	case ties > 1:
		return Port{}, fmt.Errorf("more than one controller port for %s", board)
	}
	return best, nil
}

// Learn subscribes to the board's controller port and returns the first gesture, or
// ErrLearnTimeout after the window. It always closes the source, which drops the subscription.
func Learn(ctx context.Context, src Source, board string, window time.Duration) (*Result, error) {
	defer src.Close()
	l, err := NewLearner(board)
	if err != nil {
		return nil, err
	}
	ports, err := src.Ports()
	if err != nil {
		return nil, err
	}
	port, err := PickPort(board, ports)
	if err != nil {
		return nil, err
	}
	if err := src.Subscribe(port); err != nil {
		return nil, fmt.Errorf("subscribe %d:%d: %w", port.Client, port.Port, err)
	}
	return learnLoop(ctx, src, l, window)
}

// learnLoop feeds events to the learner every few milliseconds until it has a gesture.
func learnLoop(ctx context.Context, src Source, l *Learner, window time.Duration) (*Result, error) {
	start := time.Now()
	deadline := start.Add(window)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		if !now.Before(deadline) {
			return nil, ErrLearnTimeout
		}
		wait := 20 * time.Millisecond
		if left := deadline.Sub(now); left < wait {
			wait = left
		}
		events, err := src.Next(wait)
		if err != nil {
			return nil, err
		}
		at := time.Since(start)
		for _, e := range events {
			e.At = at
			if r := l.Feed(e); r != nil {
				return r, nil
			}
		}
		if r := l.Tick(at); r != nil {
			return r, nil
		}
	}
}
