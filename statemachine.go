/*
	Copyright (c) 2013 Ondřej Kupka

	Permission is hereby granted, free of charge, to any person obtaining a copy of
	this software and associated documentation files (the "Software"), to deal in
	the Software without restriction, including without limitation the rights to
	use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
	the Software, and to permit persons to whom the Software is furnished to do so,
	subject to the following conditions:

	The above copyright notice and this permission notice shall be included in all
	copies or substantial portions of the Software.

	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
	FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
	COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
	IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
	CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package statemachine

import (
	"container/list"
	"errors"
)

// PUBLIC TYPES ---------------------------------------------------------------

type (
	State     int
	EventType int
	EventData interface{}
)

// Events are the basic units that can be processed by a state machine.
type Event struct {
	Type EventType
	Data EventData
}

// Various EventHandlers can be registered to process events in particular states.
// By registering event handlers we build up a mapping of state x event -> handler
// and the handler is invoked exactly in the defined state when the defined event
// is emitted.
//
// Once a handler is invoked, its role is to take the StateMachine into the next
// state, doing some useful work on the way.
//
// If an event is emitted in a state where no handler is defined,
// ErrIllegalEvent is returned.
type EventHandler func(s State, e *Event) (next State)

// StateMachine is the only struct this package exports. Once an event is
// emitted on a StateMachine, the relevant handler is fetched and invoked.
// StateMachine takes care of all the synchronization, it is thread-safe.
// It does not use any locking, just channels. While that may be a bit more
// overhead, it is more robust and clear.
//
// It uses an unbuffered channel for passing events to the internal goroutine,
// so all the methods block until their requests read from that channel.
type StateMachine struct {
	// Internal StateMachine state
	state State

	// Registered event handlers
	handlers [][]*list.List

	// Communication channels
	cmdCh        chan *command // Send commands to the background loop
	terminatedCh chan struct{} // Signal that the state machine is terminated
}

// CONSTRUCTOR ----------------------------------------------------------------

// Create new StateMachine. Allocate internal memory for particular number of
// states and events.
func New(initState State, stateCount, eventCount uint) *StateMachine {
	// Allocate enough space for the handlers.
	table := make([][]*list.List, stateCount)

	for i := range table {
		table[i] = make([]*list.List, eventCount)

		for j := range table[i] {
			table[i][j] = list.New()
		}
	}

	sm := StateMachine{
		state:        initState,
		handlers:     table,
		cmdCh:        make(chan *command),
		terminatedCh: make(chan struct{}),
	}

	// Start background goroutine.
	go sm.loop()

	return &sm
}

// COMMANDS -------------------------------------------------------------------

const (
	cmdOn EventType = iota
	cmdOff
	cmdIsHandlerAssigned
	cmdEmit
	cmdGetState
	cmdSetState
	cmdTerminate
)

type command struct {
	cmd  EventType
	args interface{}
}

// On & OnChain ---------------------------------------------------------------

type onArgs struct {
	s State
	t EventType
	h EventHandler
}

// Register an event handler.
func (sm *StateMachine) On(t EventType, ss []State, h EventHandler) error {
	for _, s := range ss {
		if err := sm.send(&command{
			cmdOn,
			&onArgs{s, t, h},
		}); err != nil {
			return err
		}
	}
	return nil
}

// Register the chain of event handlers.
func (sm *StateMachine) OnChain(t EventType, ss []State, hs []EventHandler) error {
	for _, h := range hs {
		if err := sm.On(t, ss, h); err != nil {
			return err
		}
	}
	return nil
}

// Off ------------------------------------------------------------------------

type offArgs struct {
	s State
	t EventType
}

// Drop the handler assigned to the requested state and event.
func (sm *StateMachine) Off(t EventType, s State) error {
	return sm.send(&command{
		cmdOff,
		&offArgs{s, t},
	})
}

// IsHandlerAssigned ----------------------------------------------------------

type isHandlerAssignedArgs struct {
	s  State
	t  EventType
	ch chan bool
}

// Check if a handler is defined for this state and event.
func (sm *StateMachine) IsHandlerAssigned(t EventType, s State) (defined bool, err error) {
	replyCh := make(chan bool, 1)
	err = sm.send(&command{
		cmdIsHandlerAssigned,
		&isHandlerAssignedArgs{s, t, replyCh},
	})
	if err != nil {
		return
	}
	defined = <-replyCh
	return
}

// Emit -----------------------------------------------------------------------

type emitArgs struct {
	e  *Event
	ch chan<- error
}

// Emit an event.
func (sm *StateMachine) Emit(event *Event) error {
	errCh := make(chan error, 1)
	err := sm.send(&command{
		cmdEmit,
		&emitArgs{event, errCh},
	})
	if err != nil {
		return err
	}
	return <-errCh
}

// GetState & SetState --------------------------------------------------------

// GetState returns the internal state machine state.
func (sm *StateMachine) GetState() (st State, err error) {
	replyCh := make(chan State, 1)
	err = sm.send(&command{
		cmdGetState,
		replyCh,
	})
	if err != nil {
		return
	}
	st = <-replyCh
	return
}

// SetState changes the internal state machine state.
func (sm *StateMachine) SetState(state State) error {
	return sm.send(&command{
		cmdSetState,
		state,
	})
}

// Terminate ------------------------------------------------------------------

// Terminate the internal event loop and close all internal channels.
// Particularly the termination channel is closed to signal all producers that
// they can no longer emit any events and shall exit.
func (sm *StateMachine) Terminate() error {
	return sm.send(&command{
		cmdTerminate,
		nil,
	})
}

// TerminateChannel can be used to obtain a channel that is closed once
// the state machine is terminated and is no longer willing to accept any events.
// This is useful if you want to start multiple goroutines to asynchronously
// post events. You can just start them, pass them this termination channel
// and leave them be. The only requirement is that those producer goroutines
// should exit or simply stop posting any events as soon as the channel is closed.
func (sm *StateMachine) TerminatedChannel() (isTerminatedCh chan struct{}) {
	return sm.terminatedCh
}

// INTERNALS ------------------------------------------------------------------

// Helper method for sending events to the internal event loop.
func (sm *StateMachine) send(cmd *command) error {
	select {
	case sm.cmdCh <- cmd:
		return nil
	case <-sm.terminatedCh:
		return ErrTerminated
	}
}

func (sm *StateMachine) handleEmit(args *emitArgs) {
	hChain := sm.handlers[sm.state][args.e.Type]

	if hChain.Len() == 0 {
		args.ch <- ErrIllegalEvent
		close(args.ch)
		return
	}
	close(args.ch)

	for e := hChain.Front(); e != nil; e = e.Next() {
		handler := e.Value.(EventHandler)

		sm.state = handler(sm.state, args.e)
	}
}

// The internal event loop processes events (commands) passed to it in
// a sequential manner.
func (sm *StateMachine) loop() {
	for {
		cmd := <-sm.cmdCh
		switch cmd.cmd {
		case cmdEmit:
			sm.handleEmit(cmd.args.(*emitArgs))
		case cmdSetState:
			sm.state = cmd.args.(State)
		case cmdGetState:
			replyCh := cmd.args.(chan State)
			replyCh <- sm.state
			close(replyCh)
		case cmdOn:
			args := cmd.args.(*onArgs)
			sm.handlers[args.s][args.t].PushBack(args.h)
		case cmdOff:
			//args := cmd.args.(*offArgs)
			//sm.handlers[args.s][args.t] = nil
		case cmdIsHandlerAssigned:
			args := cmd.args.(*isHandlerAssignedArgs)
			args.ch <- (sm.handlers[args.s][args.t].Len() != 0)
			close(args.ch)
		case cmdTerminate:
			close(sm.terminatedCh)
			return
		default:
			panic("Unknown command received")
		}
	}
}

// ERRORS ---------------------------------------------------------------------

var (
	// Returned from Emit if there is no mapping for the current state and the
	// event that is being emitted.
	ErrIllegalEvent = errors.New("Illegal event received")

	// Returned from a method if the state machine is already terminated.
	ErrTerminated = errors.New("State machine terminated")
)
