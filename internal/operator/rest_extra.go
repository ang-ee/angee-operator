package operator

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/fyltr/angee/api"
)

// REST parity handlers for operations previously reachable only via GraphQL.
// All endpoints are auth()-wrapped at mux registration time so the bearer
// token requirement matches the existing protected surface.

func (s *Server) gitOpsTopology(w http.ResponseWriter, r *http.Request) {
	withCommits := 0
	if raw := r.URL.Query().Get("with_commits"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeBadRequest(w, errors.New("with_commits must be a non-negative integer"))
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

// gitOpRequest is the JSON body accepted by merge/rebase REST endpoints.
// `Ref` carries the merge/rebase target; `Remote` and `Branch` are only
// used by `publish`.
type gitOpRequest struct {
	Ref    string `json:"ref,omitempty"`
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
}

func (s *Server) workspaceSourceMerge(w http.ResponseWriter, r *http.Request) {
	req, err := decode[gitOpRequest](r)
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
	req, err := decode[gitOpRequest](r)
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
	req, err := decode[gitOpRequest](r)
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
	descs, err := s.platform.Templates(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, descs)
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

type mintTokenRequest struct {
	Actor string `json:"actor"`
	TTL   string `json:"ttl,omitempty"`
}

func (s *Server) mintConnectionToken(w http.ResponseWriter, r *http.Request) {
	req, err := decode[mintTokenRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if s.tokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.ErrorResponse{Error: "token minter not initialised"})
		return
	}
	resp, err := s.tokens.Mint(req.Actor, req.TTL)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
