package core

import (
	"fmt"
	"pfi/sensorbee/sensorbee/core/tuple"
	"reflect"
	"sync"
)

func newDynamicPipe(inputName string, capacity int) (*dynamicPipeReceiver, *dynamicPipeSender) {
	p := make(chan *tuple.Tuple, capacity) // TODO: the type should be chan []*tuple.Tuple

	r := &dynamicPipeReceiver{
		in: p,
	}

	s := &dynamicPipeSender{
		inputName: inputName,
		out:       p,
	}
	r.sender = s
	return r, s
}

type dynamicPipeReceiver struct {
	in     <-chan *tuple.Tuple
	sender *dynamicPipeSender
}

// close closes the channel from the receiver side. It doesn't directly close
// the channel. Instead, it sends a signal to the sender so that sender can
// close the channel.
func (r *dynamicPipeReceiver) close() {
	// Close the sender because close(r.in) isn't safe.
	r.sender.close()
}

type dynamicPipeSender struct {
	inputName   string
	out         chan<- *tuple.Tuple
	inputClosed <-chan struct{}

	// rwm protects out from write-close conflicts.
	rwm    sync.RWMutex
	closed bool
}

// Write outputs the given tuple to the pipe. This method only returns
// errPipeClosed and never panics.
func (s *dynamicPipeSender) Write(ctx *Context, t *tuple.Tuple) error {
	s.rwm.RLock()
	defer s.rwm.RUnlock()

	if s.closed {
		return errPipeClosed
	}

	t.InputName = s.inputName
	s.out <- t
	return nil
}

// Close closes a channel. When multiple goroutines try to close the channel,
// only one goroutine can actually close it. Other goroutines don't wait until
// the channel is actually closed. Close never fails.
func (s *dynamicPipeSender) Close(ctx *Context) error {
	// Close method is provided to support WriteCloser interface.
	s.close()
	return nil
}

func (s *dynamicPipeSender) close() {
	s.rwm.Lock()
	defer s.rwm.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.out)
}

type dynamicDataSources struct {
	nodeName string

	// m protects recvs and recvChs.
	m sync.Mutex

	// wg waits until pour finishes.
	wg sync.WaitGroup

	recvs map[string]*dynamicPipeReceiver

	// recvChs is a slice of channels which are connected to goroutines
	// pouring tuples.
	recvChs []chan<- *dynamicPipeReceiver

	// stopCh is used to stop pouringThreads. They're signaled by close(stopCh).
	stopCh chan<- struct{}
}

func newDynamicDataSources(nodeName string) *dynamicDataSources {
	return &dynamicDataSources{
		nodeName: nodeName,
		recvs:    map[string]*dynamicPipeReceiver{},
	}
}

func (s *dynamicDataSources) add(name string, r *dynamicPipeReceiver) error {
	// Because dynamicDataSources is used internally and shouldn't return error
	// in most cases, there's no need to check s.recvs with RLock before
	// actually acquiring Lock.
	s.m.Lock()
	defer s.m.Unlock()
	if s.recvs == nil {
		return fmt.Errorf("node '%v' already closed its input", s.nodeName)
	}

	if _, ok := s.recvs[name]; ok {
		return fmt.Errorf("node '%v' is already receiving tuples from '%v'", s.nodeName, name)
	}
	s.recvs[name] = r
	for _, ch := range s.recvChs {
		ch <- r
	}
	return nil
}

func (s *dynamicDataSources) remove(name string) {
	s.m.Lock()
	defer s.m.Unlock()
	r, ok := s.recvs[name]
	if !ok {
		return
	}
	delete(s.recvs, name)
	r.close() // This eventually closes the channel.
}

// pour pours out tuples for the target Writer. The target must directly be
// connected to a Box or a Sink.
//
// callback is called after all the goroutine started if it isn't nil.
func (s *dynamicDataSources) pour(ctx *Context, w Writer, paralellism int, callback func()) error {
	if paralellism == 0 {
		paralellism = 1
	}

	var (
		logOnce   sync.Once
		threadErr error
	)

	err := func() error {
		s.m.Lock()
		defer s.m.Unlock()
		if s.recvs == nil {
			return fmt.Errorf("'%v' already stopped receiving tuples", s.nodeName)
		}

		if len(s.recvChs) != 0 {
			return fmt.Errorf("'%v' already started to receive tuples", s.nodeName)
		}

		stopCh := make(chan struct{})
		s.stopCh = stopCh

		genCases := func(newRecvCh <-chan *dynamicPipeReceiver) []reflect.SelectCase {
			cs := make([]reflect.SelectCase, 0, len(s.recvs)+1)
			cs = append(cs, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(newRecvCh),
			})
			cs = append(cs, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(stopCh),
			})

			// This case is used as a default case after stopCh is closed.
			// Currently, it only has recv with nil channel so that
			// reflect.Select does nothing on it.
			cs = append(cs, reflect.SelectCase{
				Dir: reflect.SelectRecv,
			})

			for _, r := range s.recvs {
				cs = append(cs, reflect.SelectCase{
					Dir:  reflect.SelectRecv,
					Chan: reflect.ValueOf(r.in),
				})
			}
			return cs
		}

		for i := 0; i < paralellism; i++ {
			recvCh := make(chan *dynamicPipeReceiver)
			s.recvChs = append(s.recvChs, recvCh)

			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				if err := s.pouringThread(ctx, w, genCases(recvCh)); err != nil {
					logOnce.Do(func() {
						threadErr = err // return only one error
						ctx.Logger.Log(Error, "'%v' stopped with a fatal error: %v", s.nodeName, err)
					})
				}
			}()
		}
		return nil
	}()
	if err != nil {
		return err
	}

	if callback != nil {
		callback()
	}
	s.wg.Wait()
	return threadErr
}

func (s *dynamicDataSources) pouringThread(ctx *Context, w Writer, cs []reflect.SelectCase) (retErr error) {
	const (
		newRecv = iota
		stopSignal
		defaultCase

		// maxControlIndex has the max index of special channels used to
		// control this method.
		maxControlIndex = defaultCase
	)

	defer func() {
		if e := recover(); e != nil {
			if err, ok := e.(error); ok {
				if !IsFatalError(err) {
					err = FatalError(err)
				}
				retErr = err
			} else {
				retErr = fmt.Errorf("'%v' got an unknown error through panic: %v", s.nodeName, e)
			}
		}
	}()

receiveLoop:
	for {
		i, v, ok := reflect.Select(cs) // all cases are receive direction
		if !ok && i != defaultCase {
			if i == stopSignal {
				cs[i].Chan = reflect.Value{}
				cs[defaultCase].Dir = reflect.SelectDefault // activate the default case
				continue

			} else if i <= maxControlIndex {
				return FatalError(fmt.Errorf("a controlling channel (%v) of '%v' has been closed", i, s.nodeName))
			}

			// remove the closed channel by swapping it with the last element.
			cs[i], cs[len(cs)-1] = cs[len(cs)-1], cs[i]
			cs = cs[0 : len(cs)-1]
			continue
		}

		switch i {
		case newRecv:
			c, ok := v.Interface().(*dynamicPipeReceiver)
			if !ok {
				ctx.Logger.Log(Error, "Cannot add a new receiver due to a type error")
				break
			}
			cs = append(cs, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(c.in),
			})

		case stopSignal:
			return FatalError(fmt.Errorf("a stopping channel mustn't send anything"))

		case defaultCase:
			// stop has been called and there's no additional input.
			break receiveLoop

		default:
			t, ok := v.Interface().(*tuple.Tuple)
			if !ok {
				ctx.Logger.Log(Error, "Cannot receive a tuple from a receiver due to a type error")
				break
			}

			err := w.Write(ctx, t)
			switch {
			case IsFatalError(err):
				// logging is done by pour method
				return err

			case IsTemporaryError(err):
				// TODO: retry

			default:
				// Skip this tuple
			}
		}
	}
	return nil
}

func (s *dynamicDataSources) stop(ctx *Context) {
	func() {
		s.m.Lock()
		defer s.m.Unlock()
		if s.recvs == nil || s.stopCh == nil { // already stopped or not started yet
			s.recvs = nil // stop the pipe before it starts
			return
		}

		for _, r := range s.recvs {
			// This eventually closes the channels and remove edges from
			// data sources.
			r.close()
		}
		s.recvs = nil
		close(s.stopCh)
		s.stopCh = nil
	}()
	s.wg.Wait()
}

// dynamicDataDestinations have writers connected to multiple destination nodes and
// distributes tuples to them.
type dynamicDataDestinations struct {
	// nodeName is the name of the node which writes tuples to
	// destinations (i.e. the name of a Source or a Box).
	nodeName string
	rwm      sync.RWMutex
	dsts     map[string]*dynamicPipeSender
}

func newDynamicDataDestinations(nodeName string) *dynamicDataDestinations {
	return &dynamicDataDestinations{
		nodeName: nodeName,
		dsts:     map[string]*dynamicPipeSender{},
	}
}

func (d *dynamicDataDestinations) add(name string, s *dynamicPipeSender) error {
	d.rwm.Lock()
	defer d.rwm.Unlock()
	if d.dsts == nil {
		return fmt.Errorf("node '%v' already closed its output", d.nodeName)
	}

	if _, ok := d.dsts[name]; ok {
		return fmt.Errorf("node '%v' already has the destination '%v'", d.nodeName, name)
	}
	d.dsts[name] = s
	return nil
}

func (d *dynamicDataDestinations) remove(name string) {
	d.rwm.Lock()
	defer d.rwm.Unlock()
	dst, ok := d.dsts[name]
	if !ok {
		return
	}
	delete(d.dsts, name)
	dst.close()
}

func (d *dynamicDataDestinations) Write(ctx *Context, t *tuple.Tuple) error {
	d.rwm.RLock()
	shouldUnlock := true
	defer func() {
		if shouldUnlock {
			d.rwm.RUnlock()
		}
	}()

	needsCopy := len(d.dsts) > 1
	var closed []string
	for name, dst := range d.dsts {
		// TODO: recovering from panic here instead of using RWLock in
		// dynamicPipeSender might be faster.

		s := t
		if needsCopy {
			s = t.Copy()
		}

		if err := dst.Write(ctx, s); err != nil { // never panics
			// err is always errPipeClosed when it isn't nil.
			// Because the closed destination doesn't do anything harmful,
			// it'll be removed later for performance reason.

			closed = append(closed, name)
		}
	}

	if closed != nil {
		shouldUnlock = false
		d.rwm.RUnlock()
		d.rwm.Lock()
		defer d.rwm.Unlock()
		for _, n := range closed {
			delete(d.dsts, n)
		}
	}
	return nil
}

func (d *dynamicDataDestinations) Close(ctx *Context) error {
	d.rwm.Lock()
	defer d.rwm.Unlock()
	for _, dst := range d.dsts {
		dst.close()
	}
	d.dsts = nil
	return nil
}
