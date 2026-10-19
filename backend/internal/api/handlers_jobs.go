package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/store"
)

func (s *Server) listHandlers(w http.ResponseWriter, _ *http.Request) {
	ok(w, s.jobsRegistryInfos())
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	f := store.JobFilter{
		Query:        qString(r, "q", 200),
		Statuses:     qList(r, "status"),
		Handlers:     qList(r, "handler"),
		ScheduleType: qString(r, "schedule_type", 16),
		Priority:     qInt(r, "priority", 0),
		Sort:         qString(r, "sort", 32),
		Desc:         r.URL.Query().Get("order") != "asc",
		Page:         qPage(r),
	}
	items, total, err := s.jobs.List(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	paged(w, items, f.Page, total)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var in service.JobInput
	if !s.decode(w, r, &in) {
		return
	}
	job, replayed, err := s.jobs.Create(r.Context(), principal(r), in, r.Header.Get("Idempotency-Key"))
	if err != nil {
		var ce *domain.ConflictError
		if errors.As(err, &ce) && ce.Code == "idempotency_key_reused" {
			fail(w, r, http.StatusUnprocessableEntity, ce.Code, capitalize(ce.Message)+".", nil)
			return
		}
		s.failErr(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		ok(w, job)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID.String())
	created(w, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	d, err := s.jobs.Get(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, d)
}

// updateJob requires the caller's last-seen version, either in an If-Match
// header (ETag style) or a "version" query parameter.
func (s *Server) updateJob(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	verStr := strings.Trim(r.Header.Get("If-Match"), `"W/`)
	if verStr == "" {
		verStr = r.URL.Query().Get("version")
	}
	version, err := strconv.Atoi(verStr)
	if err != nil || version < 1 {
		fail(w, r, http.StatusPreconditionRequired, "version_required", "Send the job's current version in an If-Match header.", nil)
		return
	}
	var in service.JobInput
	if !s.decode(w, r, &in) {
		return
	}
	job, err := s.jobs.Update(r.Context(), principal(r), id, in, version)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, job)
}

func (s *Server) jobAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, valid := pathUUID(w, r, "id")
		if !valid {
			return
		}
		p := principal(r)
		var job *domain.Job
		var err error
		switch action {
		case "pause":
			job, err = s.eng.PauseJob(r.Context(), p, id)
		case "resume":
			job, err = s.eng.ResumeJob(r.Context(), p, id)
		case "cancel":
			job, err = s.eng.CancelJob(r.Context(), p, id)
		}
		if err != nil {
			s.failErr(w, r, err)
			return
		}
		ok(w, job)
	}
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	run, err := s.eng.RetryJob(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	created(w, run)
}

func (s *Server) runNow(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	run, err := s.eng.RunNow(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	created(w, run)
}

type bulkRequest struct {
	Action string      `json:"action"`
	IDs    []uuid.UUID `json:"ids"`
}

func (s *Server) bulkJobs(w http.ResponseWriter, r *http.Request) {
	var in bulkRequest
	if !s.decode(w, r, &in) {
		return
	}
	results, err := s.jobs.Bulk(r.Context(), principal(r), in.Action, in.IDs)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, results)
}

func (s *Server) listJobRuns(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	if _, err := s.jobs.Get(r.Context(), principal(r), id); err != nil {
		s.failErr(w, r, err)
		return
	}
	f := store.RunFilter{JobID: &id, Statuses: qList(r, "status"), Page: qPage(r)}
	runs, total, err := s.ops.Runs(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	paged(w, runs, f.Page, total)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	v, err := s.jobs.Schedules(r.Context(), principal(r))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, v)
}

func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CronExpr string `json:"cron_expr"`
		Timezone string `json:"timezone"`
		Count    int    `json:"count"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	times, err := s.jobs.PreviewSchedule(in.CronExpr, in.Timezone, in.Count)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, map[string]any{"next_runs": times})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	d, err := s.ops.Run(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, d)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	run, err := s.eng.CancelRun(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, run)
}

func (s *Server) replayRun(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	if err := s.checkReplayable(r, id); err != nil {
		s.failErr(w, r, err)
		return
	}
	run, err := s.eng.ReplayRun(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	created(w, run)
}

// checkReplayable refuses replays of runs whose handler no longer exists.
func (s *Server) checkReplayable(r *http.Request, id uuid.UUID) error {
	d, err := s.ops.Run(r.Context(), principal(r), id)
	if err != nil {
		return err
	}
	if !s.jobs.HandlerSupported(d.Run.Handler) {
		return domain.Conflict("handler_unavailable", "handler %q is no longer available", d.Run.Handler)
	}
	return nil
}

func (s *Server) listDeadLetter(w http.ResponseWriter, r *http.Request) {
	f := store.RunFilter{DeadLettered: true, Query: qString(r, "q", 200), JobID: qUUID(r, "job_id"), Page: qPage(r)}
	runs, total, err := s.ops.Runs(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	paged(w, runs, f.Page, total)
}

func (s *Server) bulkReplay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 100 {
		fail(w, r, http.StatusUnprocessableEntity, "validation_failed", "Select between 1 and 100 runs.", map[string]string{"ids": "Select between 1 and 100 runs."})
		return
	}
	results := make([]service.BulkResult, 0, len(in.IDs))
	for _, id := range in.IDs {
		res := service.BulkResult{ID: id, OK: true}
		err := s.checkReplayable(r, id)
		if err == nil {
			_, err = s.eng.ReplayRun(r.Context(), principal(r), id)
		}
		if err != nil {
			res.OK = false
			var ce *domain.ConflictError
			switch {
			case errors.As(err, &ce):
				res.Code, res.Message = ce.Code, capitalize(ce.Message)+"."
			case errors.Is(err, domain.ErrNotFound):
				res.Code, res.Message = "not_found", "Run not found."
			default:
				s.log.Error("bulk replay failed", "run_id", id, "err", err)
				res.Code, res.Message = "internal_error", "Unexpected error."
			}
		}
		results = append(results, res)
	}
	ok(w, results)
}
