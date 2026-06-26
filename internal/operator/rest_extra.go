package operator

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ang-ee/angee-operator/api"
)

// REST parity handlers for operations previously reachable only via GraphQL.
// All endpoints are auth()-wrapped at mux registration time so the bearer
// token requirement matches the existing protected surface.

func (s *Server) gitOpsTopology(w http.ResponseWriter, r *http.Request) {
	withCommits := 0
	if raw := r.URL.Query().Get("with_commits"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeBadRequest(w, errors.New("with_commits must be an integer"))
			return
		}
		withCommits = parsed
	}
	topology, err := s.platform.GitOpsTopologyWithCommits(r.Context(), withCommits)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, topology)
}

func (s *Server) sourceDiff(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ref := r.URL.Query().Get("ref")
	files, err := s.platform.SourceDiff(r.Context(), name, ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) workspaceCreatePreflight(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceCreateRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	resp, err := s.platform.WorkspaceCreatePreflight(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) workspaceSourceFetch(w http.ResponseWriter, r *http.Request) {
	state, err := s.platform.WorkspaceSourceFetch(r.Context(), r.PathValue("name"), r.PathValue("slot"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) workspaceSourcePull(w http.ResponseWriter, r *http.Request) {
	state, err := s.platform.WorkspaceSourcePull(r.Context(), r.PathValue("name"), r.PathValue("slot"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) workspaceSourcePush(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.SourceOperationRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	state, err := s.platform.WorkspaceSourcePush(r.Context(), r.PathValue("name"), r.PathValue("slot"), req.Ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) workspaceSourceDiff(w http.ResponseWriter, r *http.Request) {
	files, err := s.platform.WorkspaceSourceDiff(r.Context(), r.PathValue("name"), r.PathValue("slot"), r.URL.Query().Get("ref"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) workspaceSourceMerge(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceSourceGitOpRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	result, err := s.platform.WorkspaceSourceMerge(r.Context(), r.PathValue("name"), r.PathValue("slot"), req.Ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaceSourceRebase(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceSourceGitOpRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	result, err := s.platform.WorkspaceSourceRebase(r.Context(), r.PathValue("name"), r.PathValue("slot"), req.Ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaceSourceMergeAbort(w http.ResponseWriter, r *http.Request) {
	result, err := s.platform.WorkspaceSourceMergeAbort(r.Context(), r.PathValue("name"), r.PathValue("slot"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaceSourceRebaseAbort(w http.ResponseWriter, r *http.Request) {
	result, err := s.platform.WorkspaceSourceRebaseAbort(r.Context(), r.PathValue("name"), r.PathValue("slot"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaceSourceRebaseContinue(w http.ResponseWriter, r *http.Request) {
	result, err := s.platform.WorkspaceSourceRebaseContinue(r.Context(), r.PathValue("name"), r.PathValue("slot"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) workspaceSourcePublish(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceSourceGitOpRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	result, err := s.platform.WorkspaceSourcePublish(r.Context(), r.PathValue("name"), r.PathValue("slot"), req.Remote, req.Branch)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) templates(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.Templates(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TemplateListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) template(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if ref == "" {
		writeBadRequest(w, errors.New("template ref is required"))
		return
	}
	desc, err := s.platform.Template(r.Context(), ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, desc)
}

func (s *Server) mintConnectionToken(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.MintConnectionTokenRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if s.tokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: "token minter not initialised"})
		return
	}
	resp, err := s.tokens.MintConnection(req.Actor, req.Scope, req.TTL)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) mintRouteToken(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.MintRouteTokenRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if s.tokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: "token minter not initialised"})
		return
	}
	resp, err := s.tokens.MintRoute(req.Actor, req.Service, req.TTL)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
