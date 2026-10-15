// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/httpapi"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface/hubtest"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/model"
)

const (
	qwenRepo   = "Qwen/Qwen2.5-14B-Instruct"
	qwenCommit = "cf98f3b3bbb457ad9e2bb7baf9a0125b6b88caa8"
	qwenSlug   = "qwen--qwen2.5-14b-instruct"
)

// seedWithout writes the committed catalog without the models slugs to the upstream layer.
func (e *env) seedWithout(slugs ...string) {
	e.t.Helper()
	in, _, err := bootstrap.Load("../../../data")
	if err != nil {
		e.t.Fatal(err)
	}
	in.Models = slices.DeleteFunc(in.Models, func(m catalog.ModelTemplate) bool { return slices.Contains(slugs, m.Slug) })
	run, err := e.s.StartImport(ctx, uuid.New(), "seed", model.LayerUpstream, "seed", "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.s.ReplaceLayer(ctx, model.LayerUpstream, run.ID, in); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.FinishImport(ctx, run.ID, "v1", nil, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) waitModelImport(id uuid.UUID) httpapi.ModelImport {
	e.t.Helper()
	var mi httpapi.ModelImport
	deadline := time.Now().Add(time.Minute)
	for {
		e.must(http.StatusOK, http.MethodGet, "/v1/model-imports/"+id.String(), e.reader, nil, &mi)
		if mi.State == httpapi.ImportSucceeded || mi.State == httpapi.ImportFailed || time.Now().After(deadline) {
			return mi
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func hubImport(repo string) httpapi.ModelImportCreate {
	return httpapi.ModelImportCreate{Source: httpapi.SourceHuggingFace, Repo: repo}
}

// A tenant's request waits until an operator approves it into the local layer, as an import run of
// its id with Hub provenance, or rejects it; the operator's own import starts queued; a repository
// another model holds is refused.
func TestModelImportApprovalIntoLocalLayer(t *testing.T) {
	h := hubtest.New(t, nil)
	e := newEnv(t, "api_model_imports", false, envOptions{hub: h.URL})
	e.seedWithout("qwen-2.5-14b")

	qwen := func(edit func(*httpapi.ModelImportCreate)) httpapi.ModelImportCreate {
		b := hubImport(qwenRepo)
		edit(&b)
		return b
	}
	for name, c := range map[string]struct {
		body httpapi.ModelImportCreate
		want int
	}{
		"modelscope":            {qwen(func(b *httpapi.ModelImportCreate) { b.Source = httpapi.SourceModelScope }), http.StatusUnprocessableEntity},
		"no owner":              {hubImport("Qwen2.5-14B-Instruct"), http.StatusBadRequest},
		"double dash":           {hubImport("a--b/c"), http.StatusBadRequest},
		"private without token": {qwen(func(b *httpapi.ModelImportCreate) { b.Private, b.TenantId = new(true), new("acme") }), http.StatusBadRequest},
		"token of a public one": {qwen(func(b *httpapi.ModelImportCreate) { b.Token = new("x") }), http.StatusBadRequest},
		"bad tenant id":         {qwen(func(b *httpapi.ModelImportCreate) { b.TenantId = new("a b") }), http.StatusBadRequest},
		"bad revision":          {qwen(func(b *httpapi.ModelImportCreate) { b.Revision = new("../main") }), http.StatusBadRequest},
	} {
		if got, body := e.call(http.MethodPost, "/v1/model-imports", e.operator, c.body); got != c.want {
			t.Errorf("%s: POST = %d %s, want %d", name, got, body, c.want)
		}
	}

	request := hubImport(qwenRepo)
	request.TenantId = new("acme")
	var requested, rejected httpapi.ModelImport
	e.must(http.StatusAccepted, http.MethodPost, "/v1/model-imports", e.operator, request, &requested)
	if requested.State != httpapi.ImportRequested || requested.TenantId != "acme" || requested.Private ||
		e.header.Get("Location") != "/v1/model-imports/"+requested.Id.String() {
		t.Errorf("tenant request %+v at %s", requested, e.header.Get("Location"))
	}
	var retried httpapi.ModelImport
	e.must(http.StatusOK, http.MethodPost, "/v1/model-imports", e.operator, request, &retried)
	if retried.Id != requested.Id || retried.State != httpapi.ImportRequested || e.header.Get("Location") != "/v1/model-imports/"+requested.Id.String() {
		t.Errorf("retried tenant request %+v, want the open one %s", retried, requested.Id)
	}
	request.Revision = new(qwenCommit)
	e.must(http.StatusAccepted, http.MethodPost, "/v1/model-imports", e.admin, request, &rejected)
	e.must(http.StatusOK, http.MethodPost, "/v1/model-imports/"+rejected.Id.String()+"/reject", e.admin,
		httpapi.ModelImportReject{Reason: new("not on our GPUs")}, &rejected)
	if rejected.State != httpapi.ImportRejected || rejected.Reason != "not on our GPUs" || rejected.DecidedAt == nil {
		t.Errorf("rejected %+v", rejected)
	}
	e.must(http.StatusConflict, http.MethodPost, "/v1/model-imports/"+rejected.Id.String()+"/approve", e.operator, nil, nil)
	var again httpapi.ModelImport
	e.must(http.StatusAccepted, http.MethodPost, "/v1/model-imports", e.admin, request, &again)
	e.must(http.StatusOK, http.MethodPost, "/v1/model-imports/"+again.Id.String()+"/reject", e.admin, nil, nil)
	if again.Id == rejected.Id {
		t.Error("a request after a rejected one answered the rejected one")
	}
	var waiting []httpapi.ModelImport
	e.must(http.StatusOK, http.MethodGet, "/v1/model-imports?tenant_id=acme&state=requested", e.reader, nil, &waiting)
	if len(waiting) != 1 || waiting[0].Id != requested.Id {
		t.Errorf("requested imports of acme %+v", waiting)
	}
	e.must(http.StatusBadRequest, http.MethodGet, "/v1/model-imports?state=lost", e.reader, nil, nil)

	var approved httpapi.ModelImport
	e.must(http.StatusOK, http.MethodPost, "/v1/model-imports/"+requested.Id.String()+"/approve", e.operator, nil, &approved)
	if approved.State != httpapi.ImportQueued && approved.State != httpapi.ImportRunning || approved.DecidedAt == nil {
		t.Errorf("approved %+v", approved)
	}
	done := e.waitModelImport(requested.Id)
	if done.State != httpapi.ImportSucceeded || done.Model != qwenSlug || done.Commit != qwenCommit || done.Report == nil ||
		done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("approved import %+v", done)
	}
	e.must(http.StatusConflict, http.MethodPost, "/v1/model-imports/"+requested.Id.String()+"/reject", e.operator, nil, nil)

	c := e.catalog(true)
	if m := modelOf(t, c, qwenSlug); m.Quantizations[0].Sources[0].Revision != qwenCommit || len(m.Presets) != 2 {
		t.Errorf("imported model %+v", m)
	}
	if p := c.Provenance.Models[qwenSlug]; len(p) != 1 || p[0].Layer != httpapi.Local || p[0].Source != "huggingface" || p[0].SourceUrl != h.URL ||
		p[0].ImportRunId == nil || *p[0].ImportRunId != requested.Id || !p[0].Effective {
		t.Errorf("provenance of %s: %+v", qwenSlug, p)
	}
	run := e.waitImport(requested.Id)
	if run.Importer != "model-import" || run.Layer != httpapi.Local || run.Status != httpapi.Succeeded || run.Summary.Changes["models"].Inserted != 1 ||
		run.InputVersion != qwenRepo+"@"+qwenCommit {
		t.Errorf("import run %+v", run)
	}
	e.generate()

	var own httpapi.ModelImport
	e.must(http.StatusAccepted, http.MethodPost, "/v1/model-imports", e.operator, hubImport(qwenRepo), &own)
	if own.State != httpapi.ImportQueued && own.State != httpapi.ImportRunning || own.TenantId != "" {
		t.Errorf("operator import %+v", own)
	}
	if done := e.waitModelImport(own.Id); done.State != httpapi.ImportSucceeded || done.Model != qwenSlug {
		t.Errorf("re-import %+v", done)
	}
	if run := e.waitImport(own.Id); len(run.Summary.Changes) != 0 {
		t.Errorf("re-importing an unchanged repository changed %+v", run.Summary.Changes)
	}

	in, _, err := bootstrap.Load("../../../data")
	if err != nil {
		t.Fatal(err)
	}
	curated := in.Models[slices.IndexFunc(in.Models, func(m catalog.ModelTemplate) bool { return m.Slug == "qwen-2.5-14b" })]
	e.must(http.StatusOK, http.MethodPut, "/v1/local/models/qwen-2.5-14b", e.operator, curated, nil)
	e.must(http.StatusAccepted, http.MethodPost, "/v1/model-imports", e.operator, hubImport(qwenRepo), &own)
	if done := e.waitModelImport(own.Id); done.State != httpapi.ImportFailed || !strings.Contains(done.Error, `in the catalog as model "qwen-2.5-14b"`) {
		t.Errorf("import of a repository another model holds: %+v", done)
	}
	e.must(http.StatusNotFound, http.MethodGet, "/v1/model-imports/"+uuid.NewString(), e.reader, nil, nil)
	e.must(http.StatusNotFound, http.MethodPost, "/v1/model-imports/"+uuid.NewString()+"/approve", e.operator, nil, nil)
}

// A private import runs within its request with the tenant's token into the tenant's layer, generated
// with fits, and never into the catalog; the token is in no table and no log line, SQL statements
// with their values included.
func TestPrivateImportIntoTenantLayer(t *testing.T) {
	const repo, token, wrong = "acme-ai/secret-qwen", "hf_tenant_secret_2c9f41", "hf_wrong_secret_77aa03"
	h := hubtest.New(t, map[string]hubtest.Private{repo: {Recorded: qwenRepo, Token: token}})
	e := newEnv(t, "api_private_import", false, envOptions{hub: h.URL, logSQL: true})
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e.importBootstrap()

	body := hubImport(repo)
	body.TenantId, body.Private, body.Token = new("acme"), new(true), new(wrong)
	var failed, mi httpapi.ModelImport
	e.must(http.StatusCreated, http.MethodPost, "/v1/model-imports", e.operator, body, &failed)
	if failed.State != httpapi.ImportFailed || !strings.Contains(failed.Error, "not found or not readable") {
		t.Errorf("private import with a wrong token %+v", failed)
	}
	body.Token = new(token)
	for range 2 {
		e.must(http.StatusCreated, http.MethodPost, "/v1/model-imports", e.operator, body, &mi)
		if mi.State != httpapi.ImportSucceeded || mi.Model != "acme-ai--secret-qwen" || !mi.Private || mi.TenantId != "acme" || mi.Commit != qwenCommit {
			t.Fatalf("private import %+v", mi)
		}
	}
	for _, a := range h.Authorizations() {
		if a != "Bearer "+token && a != "Bearer "+wrong {
			t.Errorf("a private import read the Hub with %q, want the tenant's token", a)
		}
	}

	var models []httpapi.TenantModel
	e.must(http.StatusOK, http.MethodGet, "/v1/tenants/acme/models", e.reader, nil, &models)
	if len(models) != 1 {
		t.Fatalf("tenant models %+v", models)
	}
	if m := models[0]; m.Model.Slug != mi.Model || m.ModelImport != mi.Id || m.SourceUrl != h.URL+"/"+repo || m.Model.KVBytesPerToken == 0 ||
		m.Model.Deployment.Minimum == nil || len(m.Fits) == 0 || m.Fits[0].ModelSlug != mi.Model {
		t.Errorf("tenant model %+v", m)
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/tenants/other/models", e.reader, nil, &models)
	if len(models) != 0 {
		t.Errorf("another tenant's models %+v", models)
	}
	if slices.ContainsFunc(e.catalog(false).Models, func(m httpapi.ModelTemplate) bool { return m.Slug == mi.Model }) {
		t.Error("a tenant model is in the catalog")
	}
	gen := e.generate()
	var leaked int64
	if err := e.db.Raw(`SELECT count(*) FROM generator_runs WHERE id = ? AND snapshot::text LIKE '%secret-qwen%'`, gen.Id).Scan(&leaked).Error; err != nil || leaked != 0 {
		t.Errorf("generator snapshot holds the tenant model: %d %v", leaked, err)
	}

	logged := logs.String() // before the checks below log the tokens they look for
	if !strings.Contains(logged, `INSERT INTO "tenant_models"`) {
		t.Fatal("the logs miss the SQL statements")
	}
	var tables []string
	if err := e.db.Raw(`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`).
		Scan(&tables).Error; err != nil || !slices.Contains(tables, "river_job") || !slices.Contains(tables, "model_imports") {
		t.Fatalf("tables %v %v", tables, err)
	}
	for _, secret := range []string{token, wrong} {
		for _, table := range tables {
			var n int64
			if err := e.db.Raw(`SELECT count(*) FROM "`+table+`" AS r WHERE r::text LIKE ?`, "%"+secret+"%").Scan(&n).Error; err != nil || n != 0 {
				t.Errorf("table %s holds a token in %d rows (%v)", table, n, err)
			}
		}
		if strings.Contains(logged, secret) {
			t.Errorf("a token is in the logs:\n%s", logged)
		}
	}
}

// Searches answer from the cache; inspecting shows the files, sizes, licence, gating and detected
// quantizations; private repositories and modelscope are refused.
func TestSourcesSearchAndInspect(t *testing.T) {
	h := hubtest.New(t, map[string]hubtest.Private{"operator/private": {Recorded: qwenRepo, Token: "operator-hub-token"}})
	e := newEnv(t, "api_sources", false, envOptions{hub: h.URL})
	for range 2 {
		var hits []httpapi.SourceSearchResult
		e.must(http.StatusOK, http.MethodGet, "/v1/sources/search?source=huggingface&q=qwen", e.reader, nil, &hits)
		if len(hits) != 2 || hits[0].Repo != qwenRepo || hits[0].PipelineTag != "text-generation" || hits[0].Likes != 371 {
			t.Errorf("search hits %+v", hits)
		}
	}
	if n := h.Searches(); n != 1 {
		t.Errorf("2 equal searches went upstream %d times, want 1", n)
	}
	e.must(http.StatusUnprocessableEntity, http.MethodGet, "/v1/sources/search?source=modelscope&q=qwen", e.reader, nil, nil)
	e.must(http.StatusBadRequest, http.MethodGet, "/v1/sources/search?source=huggingface&q=%20", e.reader, nil, nil)

	var m httpapi.SourceModel
	e.must(http.StatusOK, http.MethodGet, "/v1/sources/models/huggingface/"+qwenRepo, e.reader, nil, &m)
	if m.Commit != qwenCommit || m.Gated || m.License != "apache-2.0" || m.Task != "chat" || len(m.Files) != 18 || m.TotalBytes <= m.WeightsBytes ||
		len(m.Quantizations) != 1 || m.Quantizations[0].Name != "bf16" || m.Quantizations[0].Bytes != m.WeightsBytes || m.ContextMax == nil ||
		*m.ContextMax != 32768 || m.ParamsB == nil || *m.ParamsB != 14.8 || m.Card["license"] != "apache-2.0" {
		t.Errorf("inspected %+v", m)
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/sources/models/huggingface/google/gemma-2-9b-it?revision=11c9b309abf73637e4b6f9a3fa1e92e615547819",
		e.reader, nil, &m)
	if !m.Gated || m.WeightsBytes == 0 {
		t.Errorf("gated repository %+v", m)
	}
	e.must(http.StatusNotFound, http.MethodGet, "/v1/sources/models/huggingface/operator/private", e.reader, nil, nil)
	e.must(http.StatusNotFound, http.MethodGet, "/v1/sources/models/huggingface/nobody/nothing", e.reader, nil, nil)
	e.must(http.StatusUnprocessableEntity, http.MethodGet, "/v1/sources/models/modelscope/"+qwenRepo, e.reader, nil, nil)
}

// Publishing a release posts a signed release.published webhook to every subscriber, retrying a
// failed delivery with the same delivery id.
func TestReleasePublishedWebhook(t *testing.T) {
	const secret = "webhook-secret"
	var mu sync.Mutex
	var deliveries []*http.Request
	var bodies [][]byte
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		deliveries, bodies = append(deliveries, r), append(bodies, b)
		first := len(deliveries) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sub.Close)
	e := newEnv(t, "api_webhook", true, envOptions{webhooks: []jobs.Subscriber{{URL: sub.URL, Secret: secret}}, webhookBackoff: 10 * time.Millisecond})
	e.importBootstrap()
	run := e.generate()
	var rel, published httpapi.Release
	e.must(http.StatusCreated, http.MethodPost, "/v1/releases", e.operator, httpapi.ReleaseCreate{GeneratorRun: run.Id}, &rel)
	e.must(http.StatusOK, http.MethodPost, "/v1/releases/"+rel.Version+"/publish", e.operator, nil, &published)

	deadline := time.Now().Add(time.Minute)
	for {
		mu.Lock()
		n := len(deliveries)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("%d deliveries, want a failed one and its retry", len(deliveries))
	}
	_, digest, _ := strings.Cut(published.ArtifactRef, "@")
	for i, r := range deliveries {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(bodies[i])
		if got, want := r.Header.Get(jobs.SignatureHeader), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("delivery %d signature %q, want %q", i, got, want)
		}
		var ev jobs.ReleasePublished
		if err := json.Unmarshal(bodies[i], &ev); err != nil || ev != (jobs.ReleasePublished{Event: "release.published", Ref: e.ref, Digest: digest, Version: rel.Version}) ||
			!strings.HasPrefix(digest, "sha256:") {
			t.Errorf("delivery %d body %s (%v)", i, bodies[i], err)
		}
		if r.Header.Get("X-Neocloud-Event") != "release.published" || r.Header.Get("X-Neocloud-Delivery") != deliveries[0].Header.Get("X-Neocloud-Delivery") {
			t.Errorf("delivery %d headers %v", i, r.Header)
		}
	}
}
