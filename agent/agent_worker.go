package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/metrics"
	"github.com/buildkite/agent/v3/process"
	"github.com/buildkite/roko"
)

type AgentWorkerConfig struct {
	// Whether to set debug in the job
	Debug bool

	// Whether to set debugHTTP in the job
	DebugHTTP bool

	// What signal to use for worker cancellation
	CancelSignal process.Signal

	// The index of this agent worker
	SpawnIndex int

	// The configuration of the agent from the CLI
	AgentConfiguration AgentConfiguration
}

type agentStats struct {
	sync.Mutex

	// Tracks the last successful heartbeat and ping
	lastPing, lastHeartbeat time.Time

	// The last error that occurred during heartbeat, or nil if it was successful
	lastHeartbeatError error
}

type AgentWorker struct {
	stats agentStats

	// The API Client used when this agent is communicating with the API
	apiClient APIClient

	// The logger instance to use
	logger logger.Logger

	// The configuration of the agent from the CLI
	agentConfiguration AgentConfiguration

	// The registered agent API record
	agent *api.AgentRegisterResponse

	// Metric collection for the agent
	metricsCollector *metrics.Collector

	// Metrics scope for the agent
	metrics *metrics.Scope

	// Whether to enable debug
	debug bool

	// Whether to enable debugging of HTTP requests
	debugHTTP bool

	// The signal to use for cancellation
	cancelSig process.Signal

	// Stop controls
	stop      chan struct{}
	stopping  bool
	stopMutex sync.Mutex

	// The index of this agent worker
	spawnIndex int

	// When this worker runs a job, we'll store an instance of the
	// JobRunner here
	jobRunner *JobRunner

	// retrySleepFunc is useful for testing retry loops fast
	// Hopefully this can be replaced with a global setting for tests in future:
	// https://github.com/buildkite/roko/issues/2
	retrySleepFunc func(time.Duration)
}

type errUnrecoverable struct {
	action   string
	response *api.Response
	err      error
}

func (e *errUnrecoverable) Error() string {
	status := "unknown"
	if e.response != nil {
		status = e.response.Status
	}

	return fmt.Sprintf("%s failed with unrecoverable status: %s, mesage: %q", e.action, status, e.err)
}

func (e *errUnrecoverable) Is(other error) bool {
	_, ok := other.(*errUnrecoverable)
	return ok
}

func (e *errUnrecoverable) Unwrap() error {
	return e.err
}

// Creates the agent worker and initializes its API Client
func NewAgentWorker(l logger.Logger, a *api.AgentRegisterResponse, m *metrics.Collector, apiClient APIClient, c AgentWorkerConfig) *AgentWorker {
	return &AgentWorker{
		logger:             l,
		agent:              a,
		metricsCollector:   m,
		apiClient:          apiClient.FromAgentRegisterResponse(a),
		debug:              c.Debug,
		debugHTTP:          c.DebugHTTP,
		agentConfiguration: c.AgentConfiguration,
		stop:               make(chan struct{}),
		cancelSig:          c.CancelSignal,
		spawnIndex:         c.SpawnIndex,
		retrySleepFunc:     time.Sleep, // https://github.com/buildkite/roko/issues/2
	}
}

// Starts the agent worker
func (a *AgentWorker) Start(ctx context.Context, idleMonitor *IdleMonitor) error {
	a.metrics = a.metricsCollector.Scope(metrics.Tags{
		"agent_name": a.agent.Name,
	})

	// Start running our metrics collector
	if err := a.metricsCollector.Start(); err != nil {
		return err
	}
	defer a.metricsCollector.Stop()

	// Use a context to run heartbeats for as long as the agent runs for
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register our worker specific health check handler
	http.HandleFunc("/agent/"+strconv.Itoa(a.spawnIndex), func(w http.ResponseWriter, r *http.Request) {
		a.stats.Lock()
		defer a.stats.Unlock()

		if a.stats.lastHeartbeatError != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "ERROR: last heartbeat failed: %v. last successful was %v ago", a.stats.lastHeartbeatError, time.Since(a.stats.lastHeartbeat))
		} else {
			if a.stats.lastHeartbeat.IsZero() {
				fmt.Fprintf(w, "OK: no heartbeat yet")
			} else {
				fmt.Fprintf(w, "OK: last heartbeat successful %v ago", time.Since(a.stats.lastHeartbeat))
			}
		}
	})

	// Setup and start the heartbeater
	heartbeatInterval := time.Second * time.Duration(a.agent.HeartbeatInterval)
	go func() {
		for {
			select {
			case <-time.After(heartbeatInterval):
				if err := a.Heartbeat(heartbeatCtx); err != nil {
					if errors.Is(err, &errUnrecoverable{}) {
						a.logger.Error("%s", err)
						return
					}

					// Get the last heartbeat time to the nearest microsecond
					a.stats.Lock()
					if a.stats.lastHeartbeat.IsZero() {
						a.logger.Error("Failed to heartbeat %s. Will try again in %s. (No heartbeat yet)",
							err, heartbeatInterval)
					} else {
						a.logger.Error("Failed to heartbeat %s. Will try again in %s. (Last successful was %v ago)",
							err, heartbeatInterval, time.Since(a.stats.lastHeartbeat))
					}
					a.stats.Unlock()
				}

			case <-heartbeatCtx.Done():
				a.logger.Debug("Stopping heartbeats")
				return
			}
		}
	}()

	// If the agent is booted in acquisition mode, then we don't need to
	// bother about starting the ping loop.
	if a.agentConfiguration.AcquireJob != "" {
		// When in acquisition mode, there can't be any agents, so
		// there's really no point in letting the idle monitor know
		// we're busy, but it's probably a good thing to do for good
		// measure.
		idleMonitor.MarkBusy(a.agent.UUID)

		return a.AcquireAndRunJob(ctx, a.agentConfiguration.AcquireJob)
	}

	return a.startPingLoop(ctx, idleMonitor)
}

func (a *AgentWorker) startPingLoop(ctx context.Context, idleMonitor *IdleMonitor) error {
	// Create the ticker
	pingInterval := time.Second * time.Duration(a.agent.PingInterval)
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	lastActionTime := time.Now()
	a.logger.Info("Waiting for work...")

	// Continue this loop until the closing of the stop channel signals termination
	for {
		if !a.stopping {
			job, err := a.Ping(ctx)
			if err != nil {
				if errors.Is(err, &errUnrecoverable{}) {
					a.logger.Error("%v", err)
				} else {
					a.logger.Warn("%v", err)
				}
			} else if job != nil {
				// Let other agents know this agent is now busy and
				// not to idle terminate
				idleMonitor.MarkBusy(a.agent.UUID)

				// Runs the job, only errors if something goes wrong
				if runErr := a.AcceptAndRunJob(ctx, job); runErr != nil {
					a.logger.Error("%v", runErr)
				} else {
					if a.agentConfiguration.DisconnectAfterJob {
						a.logger.Info("Job finished. Disconnecting...")
						return nil
					}
					lastActionTime = time.Now()

					// Observation: jobs are rarely the last within a pipeline,
					// thus if this worker just completed a job,
					// there is likely another immediately available.
					// Skip waiting for the ping interval until
					// a ping without a job has occurred,
					// but in exchange, ensure the next ping must wait a full
					// pingInterval to avoid too much server load.

					pingTicker.Reset(pingInterval)

					continue
				}
			}

			// Handle disconnect after idle timeout (and deprecated disconnect-after-job-timeout)
			if a.agentConfiguration.DisconnectAfterIdleTimeout > 0 {
				idleDeadline := lastActionTime.Add(time.Second *
					time.Duration(a.agentConfiguration.DisconnectAfterIdleTimeout))

				if time.Now().After(idleDeadline) {
					// Let other agents know this agent is now idle and termination
					// is possible
					idleMonitor.MarkIdle(a.agent.UUID)

					// But only terminate if everyone else is also idle
					if idleMonitor.Idle() {
						a.logger.Info("All agents have been idle for %d seconds. Disconnecting...",
							a.agentConfiguration.DisconnectAfterIdleTimeout)
						return nil
					} else {
						a.logger.Debug("Agent has been idle for %.f seconds, but other agents haven't",
							time.Since(lastActionTime).Seconds())
					}
				}
			}
		}

		select {
		case <-pingTicker.C:
			continue
		case <-a.stop:
			return nil
		}
	}
}

// Stops the agent from accepting new work and cancels any current work it's
// running
func (a *AgentWorker) Stop(graceful bool) {
	// Only allow one stop to run at a time (because we're playing with
	// channels)
	a.stopMutex.Lock()
	defer a.stopMutex.Unlock()

	if graceful {
		if a.stopping {
			a.logger.Warn("Agent is already gracefully stopping...")
		} else {
			// If we have a job, tell the user that we'll wait for
			// it to finish before disconnecting
			if a.jobRunner != nil {
				a.logger.Info("Gracefully stopping agent. Waiting for current job to finish before disconnecting...")
			} else {
				a.logger.Info("Gracefully stopping agent. Since there is no job running, the agent will disconnect immediately")
			}
		}
	} else {
		// If there's a job running, kill it, then disconnect
		if a.jobRunner != nil {
			a.logger.Info("Forcefully stopping agent. The current job will be canceled before disconnecting...")

			// Kill the current job. Doesn't do anything if the job
			// is already being killed, so it's safe to call
			// multiple times.
			err := a.jobRunner.CancelAndStop()
			if err != nil {
				a.logger.Error("Unexpected error canceling job (err: %s)", err)
			}
		} else {
			a.logger.Info("Forcefully stopping agent. Since there is no job running, the agent will disconnect immediately")
		}
	}

	// We don't need to do the below operations again since we've already
	// done them before
	if a.stopping {
		return
	}

	// Use the closure of the stop channel as a signal to the main run loop in Start()
	// to stop looping and terminate
	close(a.stop)

	// Mark the agent as stopping
	a.stopping = true
}

// Connects the agent to the Buildkite Agent API, retrying up to 30 times if it
// fails.
func (a *AgentWorker) Connect(ctx context.Context) error {
	a.logger.Info("Connecting to Buildkite...")

	return roko.NewRetrier(
		roko.WithMaxAttempts(10),
		roko.WithStrategy(roko.Constant(5*time.Second)),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		_, err := a.apiClient.Connect(ctx)
		if err != nil {
			a.logger.Warn("%s (%s)", err, r)
		}
		return err
	})
}

// Performs a heatbeat
func (a *AgentWorker) Heartbeat(ctx context.Context) error {
	var beat *api.Heartbeat

	// Retry the heartbeat a few times
	err := roko.NewRetrier(
		roko.WithMaxAttempts(10),
		roko.WithStrategy(roko.Constant(5*time.Second)),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		b, resp, err := a.apiClient.Heartbeat(ctx)
		if err != nil {
			if resp != nil && !api.IsRetryableStatus(resp) {
				a.Stop(false)
				r.Break()
				return &errUnrecoverable{action: "Heartbeat", response: resp, err: err}
			}

			a.logger.Warn("%s (%s)", err, r)
			return err
		}
		beat = b
		return nil
	})

	a.stats.Lock()
	defer a.stats.Unlock()

	a.stats.lastHeartbeatError = err

	if err != nil {
		return err
	}

	// Track a timestamp for the successful heartbeat for better errors
	a.stats.lastHeartbeat = time.Now()

	a.logger.Debug("Heartbeat sent at %s and received at %s", beat.SentAt, beat.ReceivedAt)
	return nil
}

// Performs a ping that checks Buildkite for a job or action to take
// Returns a job, or nil if none is found
func (a *AgentWorker) Ping(ctx context.Context) (*api.Job, error) {
	ping, resp, pingErr := a.apiClient.Ping(ctx)
	// wait a minute, where's my if err != nil block? TL;DR look for pingErr ~20 lines down
	// the api client returns an error if the response code isn't a 2xx, but there's still information in resp and ping
	// that we need to check out to do special handling for specific error codes or messages in the response body
	// once we've done that, we can do the error handling for pingErr

	if ping != nil {
		// Is there a message that should be shown in the logs?
		if ping.Message != "" {
			a.logger.Info(ping.Message)
		}

		// Should the agent disconnect?
		if ping.Action == "disconnect" {
			a.Stop(false)
			return nil, nil
		}
	}

	if pingErr != nil {
		// If the ping has a non-retryable status, we have to kill the agent, there's no way of recovering
		// The reason we do this after the disconnect check is because the backend can (and does) send disconnect actions in
		// responses with non-retryable statuses
		if resp != nil && !api.IsRetryableStatus(resp) {
			a.Stop(false)
			return nil, &errUnrecoverable{action: "Ping", response: resp, err: pingErr}
		}

		// Get the last ping time to the nearest microsecond
		a.stats.Lock()
		defer a.stats.Unlock()

		// If a ping fails, we don't really care, because it'll
		// ping again after the interval.
		if a.stats.lastPing.IsZero() {
			return nil, fmt.Errorf("Failed to ping: %v (No successful ping yet)", pingErr)
		} else {
			return nil, fmt.Errorf("Failed to ping: %v (Last successful was %v ago)", pingErr, time.Since(a.stats.lastPing))
		}
	}

	// Track a timestamp for the successful ping for better errors
	a.stats.Lock()
	a.stats.lastPing = time.Now()
	a.stats.Unlock()

	// Should we switch endpoints?
	if ping.Endpoint != "" && ping.Endpoint != a.agent.Endpoint {
		newAPIClient := a.apiClient.FromPing(ping)

		// Before switching to the new one, do a ping test to make sure it's
		// valid. If it is, switch and carry on, otherwise ignore the switch
		newPing, _, err := newAPIClient.Ping(ctx)
		if err != nil {
			a.logger.Warn("Failed to ping the new endpoint %s - ignoring switch for now (%s)", ping.Endpoint, err)
		} else {
			// Replace the APIClient and process the new ping
			a.apiClient = newAPIClient
			a.agent.Endpoint = ping.Endpoint
			ping = newPing
		}
	}

	// If we don't have a job, there's nothing to do!
	if ping.Job == nil {
		return nil, nil
	}

	return ping.Job, nil
}

// Attempts to acquire a job and run it, only returns an error if something
// goes wrong
func (a *AgentWorker) AcquireAndRunJob(ctx context.Context, jobId string) error {
	a.logger.Info("Attempting to acquire job %s...", jobId)

	// Acquire the job using the ID we were provided. We'll retry as best
	// we can on non 422 error.
	var acquiredJob *api.Job
	err := roko.NewRetrier(
		roko.WithMaxAttempts(10),
		roko.WithStrategy(roko.Constant(3*time.Second)),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		// If this agent has been asked to stop, don't even bother
		// doing any retry checks and just bail.
		if a.stopping {
			r.Break()
		}

		var err error
		var response *api.Response

		acquiredJob, response, err = a.apiClient.AcquireJob(ctx, jobId)
		if err != nil {
			// If the API returns with a 422, that means that we
			// succesfully *tried* to acquire the job, but
			// Buildkite rejected the finish for some reason.
			if response != nil && response.StatusCode == 422 {
				a.logger.Warn("Buildkite rejected the call to acquire the job (%s)", err)
				r.Break()
			} else {
				a.logger.Warn("%s (%s)", err, r)
			}
		}

		return err
	})

	// If `acquiredJob` is nil, then the job was never acquired
	if acquiredJob == nil {
		return fmt.Errorf("Failed to acquire job: %v", err)
	}

	// Now that we've acquired the job, let's run it
	return a.RunJob(ctx, acquiredJob)
}

// Accepts a job and runs it, only returns an error if something goes wrong
func (a *AgentWorker) AcceptAndRunJob(ctx context.Context, job *api.Job) error {
	a.logger.Info("Assigned job %s. Accepting...", job.ID)

	// Accept the job. We'll retry on connection related issues, but if
	// Buildkite returns a 422 or 500 for example, we'll just bail out,
	// re-ping, and try the whole process again.
	var accepted *api.Job
	err := roko.NewRetrier(
		roko.WithMaxAttempts(30),
		roko.WithStrategy(roko.Constant(5*time.Second)),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		var err error
		accepted, _, err = a.apiClient.AcceptJob(ctx, job)
		if err != nil {
			if api.IsRetryableError(err) {
				a.logger.Warn("%s (%s)", err, r)
			} else {
				a.logger.Warn("Buildkite rejected the call to accept the job (%s)", err)
				r.Break()
			}
		}

		return err
	})

	// If `accepted` is nil, then the job was never accepted
	if accepted == nil {
		return fmt.Errorf("Failed to accept job: %v", err)
	}

	// Now that we've accepted the job, let's run it
	return a.RunJob(ctx, accepted)
}

func (a *AgentWorker) RunJob(ctx context.Context, acceptResponse *api.Job) error {
	jobMetricsScope := a.metrics.With(metrics.Tags{
		"pipeline": acceptResponse.Env["BUILDKITE_PIPELINE_SLUG"],
		"org":      acceptResponse.Env["BUILDKITE_ORGANIZATION_SLUG"],
		"branch":   acceptResponse.Env["BUILDKITE_BRANCH"],
		"source":   acceptResponse.Env["BUILDKITE_SOURCE"],
		"queue":    acceptResponse.Env["BUILDKITE_AGENT_META_DATA_QUEUE"],
	})

	// Now that we've got a job to do, we can start it.
	jr, err := NewJobRunner(a.logger, jobMetricsScope, a.agent, acceptResponse, a.apiClient, JobRunnerConfig{
		Debug:              a.debug,
		DebugHTTP:          a.debugHTTP,
		CancelSignal:       a.cancelSig,
		AgentConfiguration: a.agentConfiguration,
	})

	// Was there an error creating the job runner?
	if err != nil {
		return fmt.Errorf("Failed to initialize job: %v", err)
	}
	a.jobRunner = jr
	defer func() {
		// No more job, no more runner.
		a.jobRunner = nil
	}()

	// Start running the job
	if err := jr.Run(ctx); err != nil {
		return fmt.Errorf("Failed to run job: %v", err)
	}

	return nil
}

// Disconnect notifies the Buildkite API that this agent worker/session is
// permanently disconnecting. Don't spend long retrying, because we want to
// disconnect as fast as possible.
func (a *AgentWorker) Disconnect(ctx context.Context) error {
	a.logger.Info("Disconnecting...")
	err := roko.NewRetrier(
		roko.WithMaxAttempts(4),
		roko.WithStrategy(roko.Constant(1*time.Second)),
		roko.WithSleepFunc(a.retrySleepFunc),
	).DoWithContext(ctx, func(r *roko.Retrier) error {
		if _, err := a.apiClient.Disconnect(ctx); err != nil {
			a.logger.Warn("%s (%s)", err, r) // e.g. POST https://...: 500 (Attempt 0/4 Retrying in ..)
			return err
		}
		return nil
	})

	if err != nil {
		// none of the retries worked
		a.logger.Warn(
			"There was an error sending the disconnect API call to Buildkite. "+
				"If this agent still appears online, you may have to manually stop it (%s)",
			err,
		)
		return err
	}
	a.logger.Info("Disconnected")
	return nil
}

type IdleMonitor struct {
	sync.Mutex
	totalAgents int
	idle        map[string]struct{}
}

func NewIdleMonitor(totalAgents int) *IdleMonitor {
	return &IdleMonitor{
		totalAgents: totalAgents,
		idle:        map[string]struct{}{},
	}
}

func (i *IdleMonitor) Idle() bool {
	i.Lock()
	defer i.Unlock()
	return len(i.idle) == i.totalAgents
}

func (i *IdleMonitor) MarkIdle(agentUUID string) {
	i.Lock()
	defer i.Unlock()
	i.idle[agentUUID] = struct{}{}
}

func (i *IdleMonitor) MarkBusy(agentUUID string) {
	i.Lock()
	defer i.Unlock()
	delete(i.idle, agentUUID)
}
