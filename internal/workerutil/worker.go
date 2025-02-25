package workerutil

import (
	"context"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/derision-test/glock"
	"github.com/inconshreveable/log15"

	"github.com/sourcegraph/sourcegraph/internal/errcode"
	"github.com/sourcegraph/sourcegraph/internal/hostname"
	"github.com/sourcegraph/sourcegraph/internal/observation"
)

// ErrJobAlreadyExists occurs when a duplicate job identifier is dequeued.
var ErrJobAlreadyExists = errors.New("job already exists")

// Worker is a generic consumer of records from the workerutil store.
type Worker struct {
	store            Store
	handler          Handler
	options          WorkerOptions
	dequeueClock     glock.Clock
	heartbeatClock   glock.Clock
	shutdownClock    glock.Clock
	numDequeues      int             // tracks number of dequeue attempts
	handlerSemaphore chan struct{}   // tracks available handler slots
	ctx              context.Context // root context passed to the handler
	cancel           func()          // cancels the root context
	wg               sync.WaitGroup  // tracks active handler routines
	finished         chan struct{}   // signals that Start has finished
	runningIDSet     *IDSet          // tracks the running job IDs to heartbeat
}

type WorkerOptions struct {
	// Name denotes the name of the worker used to distinguish log messages and
	// emitted metrics. The worker constructor will fail if this field is not
	// supplied.
	Name string

	// WorkerHostname denotes the hostname of the instance/container the worker
	// is running on. If not supplied, it will be derived from either the `HOSTNAME`
	// env var, or else from os.Hostname()
	WorkerHostname string

	// NumHandlers is the maximum number of handlers that can be invoked
	// concurrently. The underlying store will not be queried while the current
	// number of handlers exceeds this value.
	NumHandlers int

	// NumTotalJobs is the maximum number of jobs that will be dequeued by the worker.
	// After this number of dequeue attempts has been made, no more dequeues will be
	// attempted. Currently dequeued jobs will finish, and the Start method of the
	// worker will unblock. If not set, there is no limit.
	NumTotalJobs int

	// MaxActiveTime is the maximum time that can be spent by the worker dequeueing
	// records to be handled. After this duration has elapsed, no more dequeues will
	// be attempted. Currently dequeued jobs will finish, and the Start method of the
	// worker will unblock. If not set, there is no limit.
	MaxActiveTime time.Duration

	// Interval is the frequency to poll the underlying store for new work.
	Interval time.Duration

	// HeartbeatInterval is the interval between heartbeat updates to a job's last_heartbeat_at field. This
	// field is periodically updated while being actively processed to signal to other workers that the
	// record is neither pending nor abandoned.
	HeartbeatInterval time.Duration

	// Metrics configures logging, tracing, and metrics for the work loop.
	Metrics WorkerMetrics
}

func NewWorker(ctx context.Context, store Store, handler Handler, options WorkerOptions) *Worker {
	clock := glock.NewRealClock()
	return newWorker(ctx, store, handler, options, clock, clock, clock)
}

func newWorker(ctx context.Context, store Store, handler Handler, options WorkerOptions, mainClock, heartbeatClock, shutdownClock glock.Clock) *Worker {
	if options.Name == "" {
		panic("no name supplied to github.com/sourcegraph/sourcegraph/internal/workerutil:newWorker")
	}

	if options.WorkerHostname == "" {
		options.WorkerHostname = hostname.Get()
	}

	ctx, cancel := context.WithCancel(ctx)

	handlerSemaphore := make(chan struct{}, options.NumHandlers)
	for i := 0; i < options.NumHandlers; i++ {
		handlerSemaphore <- struct{}{}
	}

	return &Worker{
		store:            store,
		handler:          handler,
		options:          options,
		dequeueClock:     mainClock,
		heartbeatClock:   heartbeatClock,
		shutdownClock:    shutdownClock,
		handlerSemaphore: handlerSemaphore,
		ctx:              ctx,
		cancel:           cancel,
		finished:         make(chan struct{}),
		runningIDSet:     newIDSet(),
	}
}

// Start begins polling for work from the underlying store and processing records.
func (w *Worker) Start() {
	defer close(w.finished)

	// Create a background routine that periodically writes the current time to the running records.
	// This will keep the records claimed by the active worker for a small amount of time so that
	// it will not be processed by a second worker concurrently.
	go func() {
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-w.heartbeatClock.After(w.options.HeartbeatInterval):
			}

			ids := w.runningIDSet.Slice()
			knownIDs, err := w.store.Heartbeat(w.ctx, ids)
			if err != nil {
				log15.Error("Failed to refresh heartbeats", "name", w.options.Name, "ids", ids, "error", err)
			}
			knownIDsMap := map[int]struct{}{}
			for _, id := range knownIDs {
				knownIDsMap[id] = struct{}{}
			}

			for _, id := range ids {
				if _, ok := knownIDsMap[id]; !ok {
					log15.Error("Removed unknown job from running set", "id", id)
					w.runningIDSet.Remove(id)
				}
			}
		}
	}()

	var shutdownChan <-chan time.Time
	if w.options.MaxActiveTime > 0 {
		shutdownChan = w.shutdownClock.After(w.options.MaxActiveTime)
	} else {
		shutdownChan = make(chan time.Time)
	}

	var reason string

loop:
	for {
		if w.options.NumTotalJobs != 0 && w.numDequeues >= w.options.NumTotalJobs {
			reason = "NumTotalJobs dequeued"
			break loop
		}

		ok, err := w.dequeueAndHandle()
		if err != nil {
			if w.ctx.Err() != nil && errors.Is(err, w.ctx.Err()) {
				// If the error is due to the loop being shut down, just break
				break loop
			}

			log15.Error("Failed to dequeue and handle record", "name", w.options.Name, "err", err)
		}

		delay := w.options.Interval
		if ok {
			// If we had a successful dequeue, do not wait the poll interval.
			// Just attempt to get another handler routine and process the next
			// unit of work immediately.
			delay = 0

			// Count the number of successful dequeues, but do not count only
			// attempts. As we do this on a timed loop, we will end up just
			// sloppily counting the active time instead of the number of jobs
			// (with data) that were seen.
			w.numDequeues++
		}

		select {
		case <-w.dequeueClock.After(delay):
		case <-w.ctx.Done():
			break loop
		case <-shutdownChan:
			reason = "MaxActiveTime elapsed"
			break loop
		}
	}

	log15.Info("Shutting down dequeue loop", "name", w.options.Name, "reason", reason)
	w.wg.Wait()
}

// Stop will cause the worker loop to exit after the current iteration. This is done by canceling the
// context passed to the database and the handler functions (which may cause the currently processing
// unit of work to fail). This method blocks until all handler goroutines have exited.
func (w *Worker) Stop() {
	w.cancel()
	w.Wait()
}

// Wait blocks until all handler goroutines have exited.
func (w *Worker) Wait() {
	<-w.finished
}

// Cancel cancels the handler context of the running job with the given record id.
// It will eventually be marked as failed.
func (w *Worker) Cancel(id int) {
	w.runningIDSet.Cancel(id)
}

// dequeueAndHandle selects a queued record to process. This method returns false if no such record
// can be dequeued and returns an error only on failure to dequeue a new record - no handler errors
// will bubble up.
func (w *Worker) dequeueAndHandle() (dequeued bool, err error) {
	select {
	// If we block here we are waiting for a handler to exit so that we do not
	// exceed our configured concurrency limit.
	case <-w.handlerSemaphore:
	case <-w.ctx.Done():
		return false, w.ctx.Err()
	}
	defer func() {
		if !dequeued {
			// Ensure that if we do not dequeue a record successfully we do not
			// leak from the semaphore. This will happen if the pre dequeue hook
			// fails, if the dequeue call fails, or if there are no records to
			// process.
			w.handlerSemaphore <- struct{}{}
		}
	}()

	dequeueable, extraDequeueArguments, err := w.preDequeueHook()
	if err != nil {
		return false, errors.Wrap(err, "Handler.PreDequeueHook")
	}
	if !dequeueable {
		// Hook declined to dequeue a record
		return false, nil
	}

	// Select a queued record to process and the transaction that holds it
	record, dequeued, err := w.store.Dequeue(w.ctx, w.options.WorkerHostname, extraDequeueArguments)
	if err != nil {
		return false, errors.Wrap(err, "store.Dequeue")
	}
	if !dequeued {
		// Nothing to process
		return false, nil
	}

	handleCtx, cancel := context.WithCancel(w.ctx)
	// Register the record as running so it is included in heartbeat updates.
	if !w.runningIDSet.Add(record.RecordID(), cancel) {
		return false, ErrJobAlreadyExists
	}

	w.options.Metrics.numJobs.Inc()
	log15.Debug("Dequeued record for processing", "name", w.options.Name, "id", record.RecordID())

	if hook, ok := w.handler.(WithHooks); ok {
		hook.PreHandle(handleCtx, record)
	}

	w.wg.Add(1)

	go func() {
		defer func() {
			if hook, ok := w.handler.(WithHooks); ok {
				// Don't use handleCtx here, the record is already not owned by
				// this worker anymore at this point.
				hook.PostHandle(w.ctx, record)
			}

			// Remove the record from the set of running jobs, so it is not included
			// in heartbeat updates anymore.
			defer w.runningIDSet.Remove(record.RecordID())
			w.options.Metrics.numJobs.Dec()
			w.handlerSemaphore <- struct{}{}
			w.wg.Done()
		}()

		if err := w.handle(handleCtx, record); err != nil {
			log15.Error("Failed to finalize record", "name", w.options.Name, "err", err)
		}
	}()

	return true, nil
}

// handle processes the given record. This method returns an error only if there is an issue updating
// the record to a terminal state - no handler errors will bubble up.
func (w *Worker) handle(ctx context.Context, record Record) (err error) {
	ctx, endOperation := w.options.Metrics.operations.handle.With(ctx, &err, observation.Args{})
	defer endOperation(1, observation.Args{})

	handleErr := w.handler.Handle(ctx, record)

	if errcode.IsNonRetryable(handleErr) || handleErr != nil && w.isJobCanceled(record.RecordID(), handleErr, ctx.Err()) {
		if marked, markErr := w.store.MarkFailed(w.ctx, record.RecordID(), handleErr.Error()); markErr != nil {
			return errors.Wrap(markErr, "store.MarkFailed")
		} else if marked {
			log15.Warn("Marked record as failed", "name", w.options.Name, "id", record.RecordID(), "err", handleErr)
		}
	} else if handleErr != nil {
		if marked, markErr := w.store.MarkErrored(w.ctx, record.RecordID(), handleErr.Error()); markErr != nil {
			return errors.Wrap(markErr, "store.MarkErrored")
		} else if marked {
			log15.Warn("Marked record as errored", "name", w.options.Name, "id", record.RecordID(), "err", handleErr)
		}
	} else {
		if marked, markErr := w.store.MarkComplete(w.ctx, record.RecordID()); markErr != nil {
			return errors.Wrap(markErr, "store.MarkComplete")
		} else if marked {
			log15.Debug("Marked record as complete", "name", w.options.Name, "id", record.RecordID())
		}
	}

	log15.Debug("Handled record", "name", w.options.Name, "id", record.RecordID())
	return nil
}

// isJobCanceled returns true if the job has been canceled through the Cancel interface.
// If the context is canceled, and the job is still part of the running ID set,
// we know that it has been canceled for that reason.
func (w *Worker) isJobCanceled(id int, handleErr, ctxErr error) bool {
	return errors.Is(handleErr, ctxErr) && w.runningIDSet.Has(id)
}

// preDequeueHook invokes the handler's pre-dequeue hook if it exists.
func (w *Worker) preDequeueHook() (dequeueable bool, extraDequeueArguments interface{}, err error) {
	if o, ok := w.handler.(WithPreDequeue); ok {
		return o.PreDequeue(w.ctx)
	}

	return true, nil, nil
}
