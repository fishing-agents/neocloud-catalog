// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fishman/neocloud-catalog/service/internal/broker"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// privateImportTimeout bounds a private import, which runs within its request.
const privateImportTimeout = 2 * time.Minute

var (
	// Hub ids: no "--" or ".." (the Hub refuses them), so slugs stay unique.
	repoID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	revisionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]*$`)
	tenantID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

func validRepo(repo string) bool {
	return repoID.MatchString(repo) && !strings.Contains(repo, "--") && !strings.Contains(repo, "..")
}

func toModelImport(mi model.ModelImport) ModelImport {
	return ModelImport{Id: mi.ID, Source: SourceKind(mi.Source), Repo: mi.Repo, Revision: mi.Revision, Commit: mi.Commit, TenantId: mi.TenantID,
		Private: mi.Private, State: ModelImportState(mi.State), Model: mi.ModelSlug, Reason: mi.Reason, Error: mi.Error,
		Report: orEmpty([]string(mi.Report)), CreatedAt: mi.CreatedAt, DecidedAt: mi.DecidedAt, StartedAt: mi.StartedAt, FinishedAt: mi.FinishedAt}
}

// enqueueModelImport inserts the job running import id in the store's transaction.
func (h *handlers) enqueueModelImport(ctx context.Context, id uuid.UUID) store.Enqueue {
	return func(tx *sql.Tx) error {
		_, err := h.Jobs.InsertTx(ctx, tx, jobs.ModelImportArgs{Import: id}, nil)
		return err
	}
}

func (h *handlers) CreateModelImport(w http.ResponseWriter, r *http.Request) {
	var in ModelImportCreate
	if !decode(w, r, &in) {
		return
	}
	mi := model.ModelImport{Base: model.Base{ID: uuid.New()}, Source: string(in.Source), Repo: in.Repo, State: model.ImportQueued}
	var token string
	if in.Revision != nil {
		mi.Revision = *in.Revision
	}
	if in.TenantId != nil {
		mi.TenantID = *in.TenantId
	}
	if in.Private != nil {
		mi.Private = *in.Private
	}
	if in.Token != nil {
		token = *in.Token
	}
	var bad string
	switch {
	case in.Source == SourceModelScope:
		writeError(w, http.StatusUnprocessableEntity, "unsupported_source", broker.ErrUnsupported.Error())
		return
	case !in.Source.Valid():
		bad = "source must be huggingface"
	case !validRepo(mi.Repo):
		bad = "repo must be a repository id, owner/name"
	case mi.Revision != "" && (!revisionID.MatchString(mi.Revision) || strings.Contains(mi.Revision, "..")):
		bad = "revision must be a branch, tag or commit"
	case mi.TenantID != "" && !tenantID.MatchString(mi.TenantID):
		bad = "tenant_id must be 1 to 128 letters, digits and _.:- (starting with a letter or digit)"
	case mi.Private && (mi.TenantID == "" || token == ""):
		bad = "a private import needs tenant_id and token"
	case !mi.Private && token != "":
		bad = "token is for private imports only; public imports read with the operator's source token"
	}
	if bad != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", bad)
		return
	}
	if !mi.Private && mi.TenantID != "" {
		// A retried request answers the tenant's open request of the repository and revision.
		created, err := h.Store.RequestModelImport(r.Context(), &mi)
		if err != nil {
			fail(w, r, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		w.Header().Set("Location", "/v1/model-imports/"+mi.ID.String())
		writeJSON(w, status, toModelImport(mi))
		return
	}
	var enqueue store.Enqueue
	if !mi.Private {
		enqueue = h.enqueueModelImport(r.Context(), mi.ID)
	}
	if err := h.Store.CreateModelImport(r.Context(), &mi, enqueue); err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/model-imports/"+mi.ID.String())
	if !mi.Private {
		writeJSON(w, http.StatusAccepted, toModelImport(mi))
		return
	}
	// The import outlasts server.write_timeout; it goes on if the client leaves, and its end is
	// recorded either way.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(privateImportTimeout + publishAnswerTime))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), privateImportTimeout)
	defer cancel()
	out, err := h.Broker.RunPrivate(ctx, mi.ID, token)
	if err != nil {
		// Nothing else runs a private import; the sweep fails it if this cannot.
		if ferr := h.Store.FailModelImport(context.WithoutCancel(ctx), mi.ID, "import failed: "+err.Error()); ferr != nil {
			err = errors.Join(err, ferr)
		}
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toModelImport(out))
}

func (h *handlers) ListModelImports(w http.ResponseWriter, r *http.Request, params ListModelImportsParams) {
	f := store.ModelImportFilter{Limit: 100}
	if params.Limit != nil {
		f.Limit = *params.Limit
	}
	if f.Limit < 1 || f.Limit > 1000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be 1 to 1000")
		return
	}
	if params.TenantId != nil {
		f.TenantID = *params.TenantId
	}
	if params.State != nil {
		if !params.State.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_request", "state must be requested, queued, running, succeeded, failed or rejected")
			return
		}
		f.State = string(*params.State)
	}
	imports, err := h.Store.ModelImports(r.Context(), f)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]ModelImport, len(imports))
	for i, mi := range imports {
		out[i] = toModelImport(mi)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) GetModelImport(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	mi, err := h.Store.ModelImport(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toModelImport(mi))
}

func (h *handlers) ApproveModelImport(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	mi, err := h.Store.DecideModelImport(r.Context(), id, true, "", h.enqueueModelImport(r.Context(), id))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toModelImport(mi))
}

func (h *handlers) RejectModelImport(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var in ModelImportReject
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "body: "+err.Error())
		return
	}
	var reason string
	if in.Reason != nil {
		reason = *in.Reason
	}
	mi, err := h.Store.DecideModelImport(r.Context(), id, false, reason, nil)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toModelImport(mi))
}

func (h *handlers) ListTenantModels(w http.ResponseWriter, r *http.Request, tenant string) {
	models, err := h.Store.TenantModels(r.Context(), tenant)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]TenantModel, len(models))
	for i, m := range models {
		out[i] = TenantModel{TenantId: m.TenantID, ModelImport: m.ModelImportID, Source: SourceKind(m.Source), SourceUrl: m.SourceURL,
			AsOf: m.AsOf, Model: m.Model, Fits: orEmpty(m.Fits)}
	}
	writeJSON(w, http.StatusOK, out)
}

// failSource maps a source error to 422, 429, 404 or a logged 502.
func failSource(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, broker.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "unsupported_source", err.Error())
	case errors.Is(err, broker.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(int(broker.RetryAfter.Seconds())))
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, huggingface.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	default:
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "the source failed to answer: "+err.Error())
	}
}

func (h *handlers) SearchSources(w http.ResponseWriter, r *http.Request, params SearchSourcesParams) {
	limit, q := 20, strings.TrimSpace(params.Q)
	if params.Limit != nil {
		limit = *params.Limit
	}
	switch {
	case limit < 1 || limit > 100:
		writeError(w, http.StatusBadRequest, "invalid_request", "limit must be 1 to 100")
		return
	case q == "" || len(q) > 200:
		writeError(w, http.StatusBadRequest, "invalid_request", "q must be 1 to 200 characters")
		return
	}
	hits, err := h.Broker.Search(r.Context(), string(params.Source), q, limit)
	if err != nil {
		failSource(w, r, err)
		return
	}
	out := make([]SourceSearchResult, len(hits))
	for i, hit := range hits {
		out[i] = SourceSearchResult{Repo: hit.Repo, PipelineTag: hit.PipelineTag, LibraryName: hit.Library, Downloads: hit.Downloads, Likes: hit.Likes}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) InspectSourceModel(w http.ResponseWriter, r *http.Request, source SourceKind, owner string, name string, params InspectSourceModelParams) {
	repo, revision := owner+"/"+name, ""
	if params.Revision != nil {
		revision = *params.Revision
	}
	if !validRepo(repo) || revision != "" && (!revisionID.MatchString(revision) || strings.Contains(revision, "..")) {
		writeError(w, http.StatusBadRequest, "invalid_request", "owner/name must be a repository id and revision a branch, tag or commit")
		return
	}
	m, err := h.Broker.Inspect(r.Context(), string(source), repo, revision)
	if err != nil {
		failSource(w, r, err)
		return
	}
	out := SourceModel{Source: source, Repo: m.Repo, Commit: m.Commit, License: m.License, Gated: m.Gated, PipelineTag: m.PipelineTag,
		Task: m.Task(), Tags: orEmpty(m.Tags), Card: m.Card, TotalBytes: m.TotalBytes(), Files: make([]SourceFile, len(m.Files)),
		Quantizations: []DetectedQuantization{}}
	if out.Card == nil {
		out.Card = map[string]any{}
	}
	for i, f := range m.Files {
		out.Files[i] = SourceFile{Path: f.Path, Size: f.Size}
	}
	for _, q := range m.Quantizations() {
		out.Quantizations = append(out.Quantizations, DetectedQuantization{Name: q.Name, Kind: q.Kind, Bytes: q.Bytes, Files: q.Files})
	}
	out.WeightsBytes = m.WeightsBytes()
	if m.Params > 0 {
		out.ParamsB = new(math.Round(float64(m.Params)/1e8) / 10)
	}
	if arch, contextMax := m.Architecture(); contextMax > 0 {
		out.Architecture, out.ContextMax = &arch, &contextMax
	}
	writeJSON(w, http.StatusOK, out)
}
