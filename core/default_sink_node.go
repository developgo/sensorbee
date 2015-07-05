package core

import (
	"pfi/sensorbee/sensorbee/data"
)

type defaultSinkNode struct {
	*defaultNode
	srcs *dataSources
	sink Sink

	gracefulStopEnabled     bool
	stopOnDisconnectEnabled bool
	runErr                  error
}

func (ds *defaultSinkNode) Type() NodeType {
	return NTSink
}

func (ds *defaultSinkNode) Sink() Sink {
	return ds.sink
}

func (ds *defaultSinkNode) Name() string {
	return ds.name
}

func (ds *defaultSinkNode) State() TopologyStateHolder {
	return ds.state
}

func (ds *defaultSinkNode) Input(refname string, config *SinkInputConfig) error {
	s, err := ds.topology.dataSource(refname)
	if err != nil {
		return err
	}

	if config == nil {
		config = defaultSinkInputConfig
	}

	recv, send := newPipe("output", config.capacity())
	if err := s.destinations().add(ds.name, send); err != nil {
		return err
	}
	if err := ds.srcs.add(s.Name(), recv); err != nil {
		s.destinations().remove(ds.name)
		return err
	}
	return nil
}

func (ds *defaultSinkNode) run() error {
	if err := ds.checkAndPrepareForRunning("sink"); err != nil {
		return err
	}

	defer ds.state.Set(TSStopped)
	defer func() {
		if err := ds.sink.Close(ds.topology.ctx); err != nil {
			ds.topology.ctx.Logger.Log(Error, "Sink '%v' in topology '%v' failed to stop: %v",
				ds.name, ds.topology.name, err)
		}
	}()
	ds.state.Set(TSRunning)
	return ds.srcs.pour(ds.topology.ctx, newTraceWriter(ds.sink, ETInput, ds.name), 1)
}

func (ds *defaultSinkNode) Stop() error {
	ds.stop()
	return nil
}

func (ds *defaultSinkNode) EnableGracefulStop() {
	ds.stateMutex.Lock()
	ds.gracefulStopEnabled = true
	ds.stateMutex.Unlock()
	ds.srcs.enableGracefulStop()
}

func (ds *defaultSinkNode) StopOnDisconnect() {
	ds.stateMutex.Lock()
	ds.stopOnDisconnectEnabled = true
	ds.stateMutex.Unlock()
	ds.srcs.stopOnDisconnect()
}

func (ds *defaultSinkNode) stop() {
	if stopped, err := ds.checkAndPrepareForStopping("sink"); stopped || err != nil {
		return
	}
	ds.state.Set(TSStopping)
	ds.srcs.stop(ds.topology.ctx)
	ds.state.Wait(TSStopped)
}

func (ds *defaultSinkNode) Status() data.Map {
	ds.stateMutex.Lock()
	st := ds.state.getWithoutLock()
	gstop := ds.gracefulStopEnabled
	stopOnDisconnect := ds.stopOnDisconnectEnabled
	ds.stateMutex.Unlock()

	m := data.Map{
		"state":       data.String(st.String()),
		"input_stats": ds.srcs.status(),
		"behaviors": data.Map{
			"stop_on_disconnect": data.Bool(stopOnDisconnect),
			"graceful_stop":      data.Bool(gstop),
		},
	}
	if st == TSStopped && ds.runErr != nil {
		m["error"] = data.String(ds.runErr.Error())
	}
	if s, ok := ds.sink.(Statuser); ok {
		m["sink"] = s.Status()
	}
	return m
}
