package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/store"
)

func (s *Server) jobsRegistryInfos() []handlers.Info {
	if s.handlers == nil {
		return []handlers.Info{}
	}
	return s.handlers.Infos()
}

func (s *Server) listExecutions(w http.ResponseWriter, r *http.Request) {
	f := store.AttemptFilter{
		JobID: qUUID(r, "job_id"), RunID: qUUID(r, "run_id"), WorkerID: qUUID(r, "worker_id"),
		Statuses: qList(r, "status"), Handlers: qList(r, "handler"), Query: qString(r, "q", 200),
		Sort: qString(r, "sort", 32), Desc: r.URL.Query().Get("order") != "asc", Page: qPage(r),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = &t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = &t
		}
	}
	items, total, err := s.ops.Attempts(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	paged(w, items, f.Page, total)
}

func (s *Server) getExecution(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	d, err := s.ops.Attempt(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, d)
}

func (s *Server) executionLogs(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	logs, err := s.ops.Logs(r.Context(), principal(r), id, after)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, logs)
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	ws, err := s.ops.Workers(r.Context())
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, ws)
}

func (s *Server) getWorker(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	d, err := s.ops.Worker(r.Context(), principal(r), id)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, d)
}

func (s *Server) drainWorker(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	if !s.isPlatformAdmin(principal(r)) {
		fail(w, r, http.StatusForbidden, "forbidden", "Workers are shared by every workspace; only platform administrators can drain them.", nil)
		return
	}
	if err := s.ops.DrainWorker(r.Context(), principal(r), id); err != nil {
		s.failErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.ops.Overview(r.Context(), principal(r), r.URL.Query().Get("range"))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, ov)
}

func (s *Server) systemHealth(w http.ResponseWriter, r *http.Request) {
	ok(w, struct {
		*service.Health
		LiveUpdates bool `json:"live_updates"`
		SSEClients  int  `json:"sse_clients"`
	}{Health: s.ops.Health(r.Context()), LiveUpdates: s.hub.Live(), SSEClients: s.hub.Clients()})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	f := store.EventFilter{JobID: qUUID(r, "job_id"), RunID: qUUID(r, "run_id"), WorkerID: qUUID(r, "worker_id"),
		Severities: qList(r, "severity"), Limit: qInt(r, "limit", 50)}
	events, err := s.ops.Events(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, events)
}

func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	n, err := s.ops.Notifications(r.Context(), principal(r), currentUser(r).NotificationsSeenAt)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, n)
}

func (s *Server) notificationsSeen(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.MarkNotificationsSeen(r.Context(), principal(r)); err != nil {
		s.failErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	t, err := s.ops.Tenant(r.Context(), principal(r))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, t)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.ops.Settings(r.Context(), principal(r))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, st)
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var in domain.Settings
	if !s.decode(w, r, &in) {
		return
	}
	st, err := s.ops.UpdateSettings(r.Context(), principal(r), in)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, st)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	f := store.AuditFilter{Action: qString(r, "action", 64), TargetType: qString(r, "target_type", 32),
		TargetID: qString(r, "target_id", 64), Page: qPage(r)}
	items, total, err := s.ops.Audit(r.Context(), principal(r), f)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	paged(w, items, f.Page, total)
}

// isPlatformAdmin reports whether the caller may operate shared
// infrastructure: a workspace admin whose email is in PLATFORM_ADMIN_EMAILS.
func (s *Server) isPlatformAdmin(p *domain.Principal) bool {
	if p == nil || p.Role != domain.RoleAdmin {
		return false
	}
	return slices.Contains(s.cfg.PlatformAdmins, strings.ToLower(p.Email))
}
