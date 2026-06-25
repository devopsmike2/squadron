// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// RecommendationJobStatus is the lifecycle state of an async discovery
// recommendation job. See docs/proposals/async-recommendations-design.md.
type RecommendationJobStatus string

const (
	RecJobPending   RecommendationJobStatus = "pending"
	RecJobRunning   RecommendationJobStatus = "running"
	RecJobSucceeded RecommendationJobStatus = "succeeded"
	RecJobFailed    RecommendationJobStatus = "failed"
)

// recommendationJobTTL bounds how long a finished (or abandoned) job lingers
// in the in-memory store before the reaper drops it.
const recommendationJobTTL = time.Hour

// recommendationJobMaxRuntime caps a single proposer run. It is applied to a
// context.Background()-derived context — deliberately detached from the HTTP
// request, so returning the 202 to the client does not cancel the proposer.
const recommendationJobMaxRuntime = 5 * time.Minute

// RecommendationJob is one async proposer run.
type RecommendationJob struct {
	ID         string
	Provider   string
	AccountID  string
	Status     RecommendationJobStatus
	ResultJSON json.RawMessage // marshaled success body, set on success
	Err        *scanner.HumanizedError
	HTTPStatus int // status the sync handler would have returned, on failure
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// recommendationJobStore is an in-memory, process-local store of async
// proposer jobs. The trade-off (no cross-restart / cross-replica
// durability) is intentional and documented in the design doc: the propose
// call is idempotent and read-only, so a client that 404s on poll simply
// re-submits.
type recommendationJobStore struct {
	mu   sync.Mutex
	jobs map[string]*RecommendationJob
	ttl  time.Duration
	now  func() time.Time // injectable for tests
}

// defaultRecommendationJobStore is the process-wide store shared by every
// per-request DiscoveryHandlers (the trampolines construct a fresh handler
// per request, so the store must live above them or a poll would never find
// the job a kick-off created). Tests inject a fresh store via
// WithRecommendationJobStore for isolation.
var defaultRecommendationJobStore = newRecommendationJobStore()

func newRecommendationJobStore() *recommendationJobStore {
	return &recommendationJobStore{
		jobs: make(map[string]*RecommendationJob),
		ttl:  recommendationJobTTL,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Create registers a new pending job and returns it.
func (s *recommendationJobStore) Create(provider, accountID string) *RecommendationJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	now := s.now()
	job := &RecommendationJob{
		ID:        uuid.New().String(),
		Provider:  provider,
		AccountID: accountID,
		Status:    RecJobPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.jobs[job.ID] = job
	return job
}

// Get returns a snapshot copy of the job, or (zero,false) if unknown/expired.
func (s *recommendationJobStore) Get(id string) (RecommendationJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return RecommendationJob{}, false
	}
	return *job, true
}

// proposeFunc produces a marshaled success body, or a humanized error + the
// HTTP status the sync handler would have returned, for a single job. It is
// handed a detached, time-capped context.
type proposeFunc func(ctx context.Context) (json.RawMessage, *scanner.HumanizedError, int)

// Run executes fn in a background goroutine with a detached, time-capped
// context and records the outcome on the job. Returns immediately.
func (s *recommendationJobStore) Run(jobID string, fn proposeFunc) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), recommendationJobMaxRuntime)
		defer cancel()
		s.transition(jobID, RecJobRunning, nil, 0, nil)
		res, herr, status := fn(ctx)
		if herr != nil {
			s.transition(jobID, RecJobFailed, herr, status, nil)
			return
		}
		s.transition(jobID, RecJobSucceeded, nil, 0, res)
	}()
}

func (s *recommendationJobStore) transition(id string, status RecommendationJobStatus, herr *scanner.HumanizedError, httpStatus int, res json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	job.Status = status
	job.UpdatedAt = s.now()
	if herr != nil {
		job.Err = herr
		job.HTTPStatus = httpStatus
	}
	if res != nil {
		job.ResultJSON = res
	}
}

// reapLocked drops jobs whose last update predates the TTL. Caller holds mu.
func (s *recommendationJobStore) reapLocked() {
	cutoff := s.now().Add(-s.ttl)
	for id, job := range s.jobs {
		if job.UpdatedAt.Before(cutoff) {
			delete(s.jobs, id)
		}
	}
}

// marshalRecResult encodes a success response into the (body, nil, 200) tuple
// the proposeFunc contract expects, mapping a marshal failure into a humanized
// error so a job never silently succeeds with an empty body.
func marshalRecResult(v any) (json.RawMessage, *scanner.HumanizedError, int) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &scanner.HumanizedError{
			Code:    "ResultMarshalFailed",
			Message: "Squadron could not encode the recommendations result. The error has been logged.",
		}, http.StatusInternalServerError
	}
	return b, nil, http.StatusOK
}

// recommendationJobAcceptedResponse is the 202 body returned by a kick-off.
type recommendationJobAcceptedResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// recommendationJobStatusResponse is the poll body. Result is populated only
// when Status == "succeeded"; Error only when Status == "failed".
type recommendationJobStatusResponse struct {
	JobID  string                  `json:"job_id"`
	Status string                  `json:"status"`
	Result json.RawMessage         `json:"result,omitempty"`
	Error  *scanner.HumanizedError `json:"error,omitempty"`
}

// HandleRecommendationJobStatus is the provider-agnostic poll endpoint:
// GET /discovery/recommendations/jobs/:jobID. The job id is globally unique,
// so one route serves every cloud's async recommendations.
func (h *DiscoveryHandlers) HandleRecommendationJobStatus(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("jobID"))
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingJobID",
			Message: "job id path parameter is required.",
		}})
		return
	}
	if h.recJobs == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "JobStoreNotWired",
			Message: "Squadron's recommendation job store is not configured.",
		}})
		return
	}
	job, ok := h.recJobs.Get(jobID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
			Code:    "JobNotFound",
			Message: "No recommendation job with that id. It may have expired or the server restarted — re-run Generate recommendations.",
		}})
		return
	}
	resp := recommendationJobStatusResponse{JobID: job.ID, Status: string(job.Status)}
	switch job.Status {
	case RecJobSucceeded:
		resp.Result = job.ResultJSON
	case RecJobFailed:
		resp.Error = job.Err
	}
	c.JSON(http.StatusOK, resp)
}

// WithRecommendationJobStore overrides the shared default store. Used by
// tests to isolate the async-recommendations job lifecycle.
func (h *DiscoveryHandlers) WithRecommendationJobStore(store *recommendationJobStore) *DiscoveryHandlers {
	h.recJobs = store
	return h
}
