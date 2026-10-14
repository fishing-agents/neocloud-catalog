// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/uuid"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"gorm.io/gorm"

	"github.com/fishman/neocloud-catalog/service/internal/auth"
	"github.com/fishman/neocloud-catalog/service/internal/auth/authtest"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/httpapi"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/jobs"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const audience = "catalog-service"

var ctx = context.Background()

// env is the API on its own database and queue, with a principal of each kind.
type env struct {
	t        *testing.T
	srv      *httptest.Server
	db       *gorm.DB
	s        *store.Store
	issuer   *authtest.Issuer
	operator string // API key
	admin    string // ICP token with the admin role
	reader   string // ICP token without it
	ref      string // where publish pushes
	handler  http.Handler
	header   http.Header // of the last response
}

// newEnv starts the API; withPublish publishes to an in-memory registry with a fresh cosign key.
func newEnv(t *testing.T, suffix string, withPublish bool) *env {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	s := store.New(db)
	imp, err := bootstrap.New("../../../data")
	if err != nil {
		t.Fatal(err)
	}
	reg := importers.NewRegistry(imp)
	c, err := jobs.New(sqlDB, s, reg, jobs.Config{JobTimeout: time.Minute, RescueAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })

	iss := authtest.NewIssuer(t)
	key, _, err := auth.Issue(ctx, db, "ops", "ops", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, s: s, issuer: iss, operator: key,
		admin: iss.Token(t, iss.Claims(audience, "viewer", auth.RoleAdmin)), reader: iss.Token(t, iss.Claims(audience, "viewer"))}
	cfg := httpapi.Config{Store: s, Auth: auth.New(db, auth.OIDCConfig{IssuerURL: iss.URL, Audience: audience}), Jobs: c, Importers: reg,
		PublishTimeout: time.Minute}
	if withPublish {
		reg := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
		t.Cleanup(reg.Close)
		e.ref = strings.TrimPrefix(reg.URL, "http://") + "/neocloud/catalog:v4"
		password := []byte("test password")
		priv, _, err := cryptoutils.GeneratePEMEncodedECDSAKeyPair(elliptic.P256(), cryptoutils.StaticPasswordFunc(password))
		if err != nil {
			t.Fatal(err)
		}
		signer, err := release.NewKeySigner(priv, password)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Publish = &release.Target{Ref: e.ref, Signer: signer}
	}
	e.handler = httpapi.New(cfg)
	e.srv = httptest.NewServer(e.handler)
	t.Cleanup(e.srv.Close)
	return e
}

// call sends body as JSON with token ("" sends none), checks the response
// against the OpenAPI contract and returns its status and body.
func (e *env) call(method, path, token string, body any) (int, []byte) {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	checkContract(e.t, req, res, b)
	e.header = res.Header
	return res.StatusCode, b
}

// must is call expecting status want, decoding the body into out if non-nil.
func (e *env) must(want int, method, path, token string, body, out any) {
	e.t.Helper()
	got, b := e.call(method, path, token, body)
	if got != want {
		e.t.Fatalf("%s %s = %d %s, want %d", method, path, got, b, want)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
	}
}

var contract = sync.OnceValues(func() (routers.Router, error) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, err
	}
	doc.Servers = nil // match paths on any host
	return legacy.NewRouter(doc)
})

// checkContract fails unless the contract documents the response and its body schema.
func checkContract(t *testing.T, req *http.Request, res *http.Response, body []byte) {
	t.Helper()
	router, err := contract()
	if err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	// The router matches the decoded path; an escaped slash (node type ids) must
	// stay inside its segment.
	match := req.Clone(req.Context())
	match.URL.Path, match.URL.RawPath = strings.ReplaceAll(req.URL.EscapedPath(), "%2F", "~"), ""
	route, params, err := router.FindRoute(match)
	if err != nil {
		t.Errorf("%s %s: no operation in the contract: %v", req.Method, req.URL.Path, err)
		return
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: match, PathParams: params, Route: route},
		Status:                 res.StatusCode,
		Header:                 res.Header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Errorf("%s %s -> %d breaks the contract: %v\n%s", req.Method, req.URL.Path, res.StatusCode, err, body)
	}
}

func (e *env) importBootstrap() httpapi.ImportRun {
	e.t.Helper()
	var queued httpapi.ImportRun
	e.must(http.StatusAccepted, http.MethodPost, "/v1/imports", e.operator, httpapi.ImportCreate{Importer: "bootstrap-from-json"}, &queued)
	run := e.waitImport(queued.Id)
	if run.Status != httpapi.Succeeded {
		e.t.Fatalf("bootstrap import %+v", run)
	}
	return run
}

func (e *env) waitImport(id uuid.UUID) httpapi.ImportRun {
	e.t.Helper()
	var run httpapi.ImportRun
	deadline := time.Now().Add(time.Minute)
	for {
		e.must(http.StatusOK, http.MethodGet, "/v1/imports/"+id.String(), e.reader, nil, &run)
		if run.Status == httpapi.Succeeded || run.Status == httpapi.Failed || time.Now().After(deadline) {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *env) generate() httpapi.GeneratorRun {
	e.t.Helper()
	var run httpapi.GeneratorRun
	e.must(http.StatusAccepted, http.MethodPost, "/v1/generator-runs", e.admin, nil, &run)
	deadline := time.Now().Add(time.Minute)
	for run.Status != httpapi.Succeeded && run.Status != httpapi.Failed && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		e.must(http.StatusOK, http.MethodGet, "/v1/generator-runs/"+run.Id.String(), e.reader, nil, &run)
	}
	if run.Status != httpapi.Succeeded {
		e.t.Fatalf("generator run %+v", run)
	}
	return run
}

func (e *env) catalog(provenance bool) httpapi.EffectiveCatalog {
	e.t.Helper()
	var c httpapi.EffectiveCatalog
	path := "/v1/catalog"
	if provenance {
		path += "?provenance=1"
	}
	e.must(http.StatusOK, http.MethodGet, path, e.reader, nil, &c)
	return c
}

// Every operation refuses a request without a valid token, and every write a read-only principal.
func TestEveryOperationNeedsAuthentication(t *testing.T) {
	e := newEnv(t, "api_authn", false)
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ops := 0
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops++
			p := path
			for _, param := range append(item.Parameters, op.Parameters...) {
				v := "x"
				if s := param.Value.Schema.Value; s.Format == "uuid" {
					v = uuid.NewString()
				}
				p = strings.ReplaceAll(p, "{"+param.Value.Name+"}", v)
			}
			for name, token := range map[string]string{"no token": "", "unknown key": auth.KeyPrefix + "ops_" + strings.Repeat("x", 43),
				"foreign token": authtest.Sign(t, authtest.NewKey(t), e.issuer.Claims(audience, auth.RoleAdmin))} {
				if got, b := e.call(method, p, token, nil); got != http.StatusUnauthorized {
					t.Errorf("%s %s with %s = %d %s, want 401", method, p, name, got, b)
				}
			}
			if method != http.MethodGet {
				if got, b := e.call(method, p, e.reader, nil); got != http.StatusForbidden {
					t.Errorf("%s %s by a reader = %d %s, want 403", method, p, got, b)
				}
			}
		}
	}
	if ops < 20 {
		t.Fatalf("checked %d operations; the contract has more", ops)
	}
}

// Reads take any principal; writes take an operator key or an admin-role token.
// A refused write changes nothing.
func TestAuthMatrix(t *testing.T) {
	e := newEnv(t, "api_authz", false)
	expired, _, err := auth.Issue(ctx, e.db, "old", "old", new(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	const nodeType = "/v1/local/node-types/other%2Fmatrix"
	body := httpapi.NodeType{ID: "other/matrix", Family: "H100", GPUs: 8}
	for _, c := range []struct {
		name        string
		token       string
		read, write int
	}{
		{"no token", "", http.StatusUnauthorized, http.StatusUnauthorized},
		{"expired key", expired, http.StatusUnauthorized, http.StatusUnauthorized},
		{"token for another audience", e.issuer.Token(t, e.issuer.Claims("neocloud-api", auth.RoleAdmin)), http.StatusUnauthorized, http.StatusUnauthorized},
		{"expired token", e.issuer.Token(t, func() map[string]any {
			c := e.issuer.Claims(audience, auth.RoleAdmin)
			c["exp"] = time.Now().Add(-time.Minute).Unix()
			return c
		}()), http.StatusUnauthorized, http.StatusUnauthorized},
		{"reader", e.reader, http.StatusOK, http.StatusForbidden},
		{"admin", e.admin, http.StatusOK, http.StatusOK},
		{"operator key", e.operator, http.StatusOK, http.StatusOK},
	} {
		if got, b := e.call(http.MethodGet, "/v1/catalog", c.token, nil); got != c.read {
			t.Errorf("%s: GET /v1/catalog = %d %s, want %d", c.name, got, b, c.read)
		}
		if c.write != http.StatusOK {
			if got, b := e.call(http.MethodPut, nodeType, c.token, body); got != c.write {
				t.Errorf("%s: PUT %s = %d %s, want %d", c.name, nodeType, got, b, c.write)
			}
			e.must(http.StatusNotFound, http.MethodGet, nodeType, e.operator, nil, nil)
			continue
		}
		e.must(http.StatusOK, http.MethodPut, nodeType, c.token, body, nil)
		e.must(http.StatusNoContent, http.MethodDelete, nodeType, c.token, nil, nil)
	}
}

// Local curation overrides upstream with provenance; deleting it restores upstream.
func TestCurationRoundTrip(t *testing.T) {
	e := newEnv(t, "api_curation", false)
	imported := e.importBootstrap()
	if len(imported.Summary.Changes) == 0 || imported.StartedAt == nil || imported.FinishedAt == nil {
		t.Fatalf("bootstrap run %+v", imported)
	}

	const slug = "llama-3.1-8b"
	before := e.catalog(true)
	upstream := modelOf(t, before, slug)
	if p := before.Provenance.Models[slug]; len(p) != 1 || p[0].Layer != httpapi.Upstream || !p[0].Effective ||
		p[0].ImportRunId == nil || *p[0].ImportRunId != imported.Id || p[0].Fields["name"].Source != p[0].Source {
		t.Fatalf("upstream provenance of %s: %+v", slug, p)
	}
	if before.Provenance.Quantizations[slug+"/bf16"] == nil || before.Provenance.NodeTypes["azure/Standard_ND96isr_H100_v5"] == nil {
		t.Errorf("provenance lacks quantizations or node types: %+v", before.Provenance)
	}

	e.must(http.StatusNotFound, http.MethodGet, "/v1/local/models/"+slug, e.reader, nil, nil)
	e.must(http.StatusNotFound, http.MethodPut, "/v1/local/models/"+slug+"/presets/"+upstream.Presets[0].Name, e.admin, upstream.Presets[0], nil)

	local := upstream
	local.Name = "Llama 3.1 8B (curated)"
	local.Presets = local.Presets[:2]
	var stored httpapi.ModelTemplate
	e.must(http.StatusOK, http.MethodPut, "/v1/local/models/"+slug, e.admin, local, &stored)
	if stored.Name != local.Name || len(stored.Presets) != 2 {
		t.Errorf("stored local model %+v", stored)
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/local/models/"+slug, e.reader, nil, &stored)
	if stored.Name != local.Name {
		t.Errorf("GET local model name %q", stored.Name)
	}

	preset := upstream.Presets[3]
	preset.Name, preset.Context = "vllm-fp8-curated", 4096
	e.must(http.StatusOK, http.MethodPut, "/v1/local/models/"+slug+"/presets/"+preset.Name, e.operator, preset, nil)
	var gotPreset httpapi.Preset
	e.must(http.StatusOK, http.MethodGet, "/v1/local/models/"+slug+"/presets/"+preset.Name, e.reader, nil, &gotPreset)
	if gotPreset.Context != 4096 {
		t.Errorf("curated preset %+v", gotPreset)
	}

	after := e.catalog(true)
	m := modelOf(t, after, slug)
	if m.Name != local.Name || len(m.Presets) != 3 || m.Presets[2].Name != preset.Name {
		t.Errorf("effective %s: name %q, presets %d (%+v)", slug, m.Name, len(m.Presets), m.Presets)
	}
	if after.Models[0].Slug != slug {
		t.Errorf("the curated model moved from its upstream position to %s", after.Models[0].Slug)
	}
	p := after.Provenance.Models[slug]
	if len(p) != 2 || p[0].Layer != httpapi.Local || p[0].Source != "curation" || !p[0].Effective || p[0].ImportRunId != nil ||
		p[1].Layer != httpapi.Upstream || p[1].Effective {
		t.Errorf("provenance of the overridden %s: %+v", slug, p)
	}

	e.must(http.StatusNoContent, http.MethodDelete, "/v1/local/models/"+slug+"/presets/"+preset.Name, e.admin, nil, nil)
	e.must(http.StatusNotFound, http.MethodGet, "/v1/local/models/"+slug+"/presets/"+preset.Name, e.reader, nil, nil)
	e.must(http.StatusNotFound, http.MethodDelete, "/v1/local/models/"+slug+"/presets/"+preset.Name, e.admin, nil, nil)
	e.must(http.StatusNoContent, http.MethodDelete, "/v1/local/models/"+slug, e.admin, nil, nil)
	e.must(http.StatusNotFound, http.MethodDelete, "/v1/local/models/"+slug, e.admin, nil, nil)
	restored := e.catalog(true)
	if m := modelOf(t, restored, slug); m.Name != upstream.Name || len(m.Presets) != len(upstream.Presets) {
		t.Errorf("deleting the override left %q with %d presets", m.Name, len(m.Presets))
	}
	if p := restored.Provenance.Models[slug]; len(p) != 1 || p[0].Layer != httpapi.Upstream || !p[0].Effective {
		t.Errorf("provenance after delete: %+v", p)
	}

	const nt = "azure/Standard_ND96isr_H100_v5"
	path := "/v1/local/node-types/" + url.PathEscape(nt)
	e.must(http.StatusOK, http.MethodPut, path, e.operator, httpapi.NodeType{ID: nt, Family: "H100", GPUs: 4, Source: "https://example.com"}, nil)
	if n := nodeTypeOf(t, e.catalog(false), nt); n.GPUs != 4 {
		t.Errorf("effective node type %+v, want the local 4 GPUs", n)
	}
	e.must(http.StatusNoContent, http.MethodDelete, path, e.operator, nil, nil)
	if n := nodeTypeOf(t, e.catalog(false), nt); n.GPUs != 8 {
		t.Errorf("node type after delete %+v, want the upstream 8 GPUs", n)
	}

	for name, c := range map[string]struct {
		path string
		body any
	}{
		"model slug differs":  {"/v1/local/models/other", local},
		"preset name differs": {"/v1/local/models/" + slug + "/presets/other", preset},
		"node type differs":   {"/v1/local/node-types/other%2Fx", httpapi.NodeType{ID: "other/y", Family: "H100", GPUs: 1}},
		"not json":            {"/v1/local/models/" + slug, "{"},
	} {
		if got, b := e.call(http.MethodPut, c.path, e.admin, c.body); got != http.StatusBadRequest {
			t.Errorf("%s: PUT %s = %d %s, want 400", name, c.path, got, b)
		}
	}
}

// Prices are replaced as a whole: an unchanged price keeps its as_of, invalid ones are refused.
func TestLocalPrices(t *testing.T) {
	e := newEnv(t, "api_prices", false)
	var got []httpapi.Price
	e.must(http.StatusOK, http.MethodGet, "/v1/local/prices", e.reader, nil, &got)
	if got == nil || len(got) != 0 {
		t.Fatalf("prices of an empty layer: %+v", got)
	}
	h100 := httpapi.Price{NodeTypeId: "azure/Standard_ND96isr_H100_v5", Region: "eastus", Billing: httpapi.OnDemand, UsdPerHour: 98.32}
	spot := h100
	spot.Billing, spot.UsdPerHour = httpapi.Spot, 39.5
	e.must(http.StatusOK, http.MethodPut, "/v1/local/prices", e.admin, []httpapi.Price{h100, spot}, &got)
	if len(got) != 2 || got[0].AsOf == nil {
		t.Fatalf("stored prices %+v", got)
	}
	asOf := *got[0].AsOf
	spot.UsdPerHour = 41
	e.must(http.StatusOK, http.MethodPut, "/v1/local/prices", e.admin, []httpapi.Price{h100, spot}, &got)
	e.must(http.StatusOK, http.MethodGet, "/v1/local/prices", e.reader, nil, &got)
	if len(got) != 2 || !got[0].AsOf.Equal(asOf) || got[1].UsdPerHour != 41 || got[1].AsOf.Equal(asOf) {
		t.Errorf("prices after changing spot: %+v (first as_of %s)", got, asOf)
	}

	bad := spot
	bad.Billing = "monthly"
	negative := spot
	negative.UsdPerHour = -1
	for name, body := range map[string][]httpapi.Price{"billing": {bad}, "duplicate": {h100, h100}, "negative": {negative}, "no region": {{NodeTypeId: "x", Billing: httpapi.Spot}}} {
		if code, b := e.call(http.MethodPut, "/v1/local/prices", e.admin, body); code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %s, want 400", name, code, b)
		}
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/local/prices", e.reader, nil, &got)
	if len(got) != 2 {
		t.Errorf("a refused PUT changed the prices: %+v", got)
	}
	if code, b := e.call(http.MethodPut, "/v1/local/prices", e.admin, json.RawMessage("null")); code != http.StatusBadRequest {
		t.Errorf("PUT null = %d %s, want 400", code, b)
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/local/prices", e.reader, nil, &got)
	if len(got) != 2 {
		t.Errorf("PUT null changed the prices: %+v", got)
	}
	e.must(http.StatusNoContent, http.MethodDelete, "/v1/local/prices", e.admin, nil, nil)
	e.must(http.StatusOK, http.MethodGet, "/v1/local/prices", e.reader, nil, &got)
	if len(got) != 0 {
		t.Errorf("prices after DELETE: %+v", got)
	}
}

// Extension payloads come from the signed overlay only: local writes carrying
// any are refused and store nothing.
func TestLocalWritesRefuseExtensions(t *testing.T) {
	e := newEnv(t, "api_extensions", false)
	e.importBootstrap()
	const slug = "gemma-2-9b"
	m := modelOf(t, e.catalog(false), slug)
	ext := httpapi.Extensions{"neocloud.io/interruption": json.RawMessage(`{"tier":"gold"}`)}

	withModel := m
	withModel.Extensions = ext
	withPreset := m
	withPreset.Presets = append([]httpapi.Preset(nil), m.Presets...)
	withPreset.Presets[1].Extensions = ext
	for name, body := range map[string]httpapi.ModelTemplate{"model": withModel, "preset": withPreset} {
		if got, b := e.call(http.MethodPut, "/v1/local/models/"+slug, e.operator, body); got != http.StatusBadRequest {
			t.Errorf("payload on the %s: PUT = %d %s, want 400", name, got, b)
		}
	}
	e.must(http.StatusNotFound, http.MethodGet, "/v1/local/models/"+slug, e.reader, nil, nil)

	e.must(http.StatusOK, http.MethodPut, "/v1/local/models/"+slug, e.operator, m, nil)
	p := m.Presets[0]
	p.Extensions = ext
	if got, b := e.call(http.MethodPut, "/v1/local/models/"+slug+"/presets/"+p.Name, e.operator, p); got != http.StatusBadRequest {
		t.Errorf("payload on a preset: PUT = %d %s, want 400", got, b)
	}
	var stored httpapi.Preset
	e.must(http.StatusOK, http.MethodGet, "/v1/local/models/"+slug+"/presets/"+p.Name, e.reader, nil, &stored)
	if stored.Extensions != nil {
		t.Errorf("stored preset carries %s", stored.Extensions)
	}
}

// Runs, releases and publishing work end to end; an older release is refused
// once a newer one is published.
func TestRunsAndReleases(t *testing.T) {
	e := newEnv(t, "api_releases", true)
	imported := e.importBootstrap()
	var runs []httpapi.ImportRun
	e.must(http.StatusOK, http.MethodGet, "/v1/imports?importer=bootstrap-from-json", e.reader, nil, &runs)
	if len(runs) != 1 || runs[0].Id != imported.Id {
		t.Errorf("GET /v1/imports = %+v", runs)
	}
	e.must(http.StatusNotFound, http.MethodPost, "/v1/imports", e.operator, httpapi.ImportCreate{Importer: "nope"}, nil)
	e.must(http.StatusBadRequest, http.MethodPost, "/v1/imports", e.operator, httpapi.ImportCreate{}, nil)
	e.must(http.StatusNotFound, http.MethodGet, "/v1/imports/"+uuid.NewString(), e.reader, nil, nil)
	e.must(http.StatusBadRequest, http.MethodGet, "/v1/imports/not-a-uuid", e.reader, nil, nil)

	older := e.generate()
	if len(older.InputImportRuns) != 1 || older.InputImportRuns[0] != imported.Id || older.Report == nil {
		t.Errorf("generator run %+v", older)
	}
	if err := e.db.Exec(`UPDATE generator_runs SET snapshot = jsonb_set(snapshot, '{generated_at}', '"2026-01-01T00:00:00Z"') WHERE id = ?`, older.Id).Error; err != nil {
		t.Fatal(err)
	}
	newer := e.generate()

	var oldRel, newRel httpapi.Release
	e.must(http.StatusCreated, http.MethodPost, "/v1/releases", e.operator, httpapi.ReleaseCreate{GeneratorRun: older.Id}, &oldRel)
	e.must(http.StatusCreated, http.MethodPost, "/v1/releases", e.admin, httpapi.ReleaseCreate{GeneratorRun: newer.Id}, &newRel)
	e.must(http.StatusConflict, http.MethodPost, "/v1/releases", e.admin, httpapi.ReleaseCreate{GeneratorRun: newer.Id}, nil)
	e.must(http.StatusNotFound, http.MethodPost, "/v1/releases", e.admin, httpapi.ReleaseCreate{GeneratorRun: uuid.New()}, nil)
	if oldRel.Version >= newRel.Version || newRel.GeneratorRun != newer.Id || newRel.PublishedAt != nil {
		t.Fatalf("releases %+v and %+v", oldRel, newRel)
	}

	// The publish answers although it outlasts the server's write timeout.
	short := httptest.NewUnstartedServer(e.handler)
	short.Config.WriteTimeout = time.Millisecond
	short.Start()
	defer short.Close()
	defaultSrv := e.srv
	e.srv = short
	var published httpapi.Release
	e.must(http.StatusOK, http.MethodPost, "/v1/releases/"+newRel.Version+"/publish", e.operator, nil, &published)
	e.srv = defaultSrv
	if published.PublishedAt == nil || !strings.HasPrefix(published.ArtifactRef, strings.TrimSuffix(e.ref, ":v4")+"@sha256:") {
		t.Errorf("published release %+v", published)
	}
	var again httpapi.Release
	e.must(http.StatusOK, http.MethodPost, "/v1/releases/"+newRel.Version+"/publish", e.operator, nil, &again)
	if !again.PublishedAt.Equal(*published.PublishedAt) || again.ArtifactRef != published.ArtifactRef {
		t.Errorf("re-publishing changed the release: %+v", again)
	}
	e.must(http.StatusConflict, http.MethodPost, "/v1/releases/"+oldRel.Version+"/publish", e.operator, nil, nil)
	e.must(http.StatusNotFound, http.MethodPost, "/v1/releases/2001-01-01T00:00:00Z/publish", e.operator, nil, nil)

	var releases []httpapi.Release
	e.must(http.StatusOK, http.MethodGet, "/v1/releases", e.reader, nil, &releases)
	if len(releases) != 2 || releases[0].Version != newRel.Version || releases[0].PublishedAt == nil || releases[1].PublishedAt != nil {
		t.Errorf("GET /v1/releases = %+v", releases)
	}

	unconfigured := newEnv(t, "api_releases_unconfigured", false)
	unconfigured.must(http.StatusServiceUnavailable, http.MethodPost, "/v1/releases/"+newRel.Version+"/publish", unconfigured.operator, nil, nil)
}

// An abandoned run reads as failed, with empty lists and summary as [] and {}.
func TestAbandonedRunsReadFailed(t *testing.T) {
	e := newEnv(t, "api_abandoned", false)
	importRun, err := e.s.StartImport(ctx, uuid.New(), "bootstrap-from-json", "upstream", "bootstrap-from-json", "")
	if err != nil {
		t.Fatal(err)
	}
	genRun, err := e.s.StartGeneratorRun(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"import_runs", "generator_runs"} {
		if err := e.db.Exec(`UPDATE ` + table + ` SET started_at = now() - interval '2 hours'`).Error; err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.s.FailAbandonedRuns(ctx, time.Hour); err != nil || n != 2 {
		t.Fatalf("FailAbandonedRuns = %d, %v; want 2", n, err)
	}

	var imp httpapi.ImportRun
	e.must(http.StatusOK, http.MethodGet, "/v1/imports/"+importRun.ID.String(), e.reader, nil, &imp)
	if imp.Status != httpapi.Failed || imp.Error != store.AbandonedError || imp.Summary.Changes == nil || imp.FinishedAt == nil {
		t.Errorf("abandoned import run %+v", imp)
	}
	_, raw := e.call(http.MethodGet, "/v1/generator-runs/"+genRun.ID.String(), e.reader, nil)
	var gen httpapi.GeneratorRun
	if err := json.Unmarshal(raw, &gen); err != nil {
		t.Fatal(err)
	}
	if gen.Status != httpapi.Failed || gen.Error != store.AbandonedError || !bytes.Contains(raw, []byte(`"report":[]`)) ||
		!bytes.Contains(raw, []byte(`"input_import_runs":[]`)) {
		t.Errorf("abandoned generator run %s", raw)
	}
}

// Queued runs read as queued until a worker starts them.
func TestQueuedRuns(t *testing.T) {
	sqlDB, db := dbtest.Fresh(t, "api_queued")
	dbtest.Migrate(t, sqlDB)
	if err := jobs.Migrate(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
	s := store.New(db)
	imp, err := bootstrap.New("../../../data")
	if err != nil {
		t.Fatal(err)
	}
	reg := importers.NewRegistry(imp)
	reg.Refuse("epoch", "an operator deployment imports its upstream layer from upstream.ref only")
	c, err := jobs.New(sqlDB, s, reg, jobs.Config{JobTimeout: time.Minute, RescueAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := auth.Issue(ctx, db, "ops", "ops", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, s: s, operator: key}
	e.srv = httptest.NewServer(httpapi.New(httpapi.Config{Store: s, Auth: auth.New(db, auth.OIDCConfig{}), Jobs: c, Importers: reg}))
	t.Cleanup(e.srv.Close)

	var queued httpapi.ImportRun
	e.must(http.StatusAccepted, http.MethodPost, "/v1/imports", key, httpapi.ImportCreate{Importer: "bootstrap-from-json"}, &queued)
	if queued.Status != httpapi.Queued || queued.Importer != "bootstrap-from-json" || queued.Layer != httpapi.Upstream {
		t.Errorf("queued import %+v", queued)
	}
	if code, body := e.call(http.MethodPost, "/v1/imports", key, httpapi.ImportCreate{Importer: "epoch"}); code != http.StatusConflict ||
		!bytes.Contains(body, []byte("upstream.ref")) {
		t.Errorf("POST a refused importer = %d %s, want 409 saying why", code, body)
	}
	e.must(http.StatusNotFound, http.MethodPost, "/v1/imports", key, httpapi.ImportCreate{Importer: "no-such-importer"}, nil)
	var got httpapi.ImportRun
	e.must(http.StatusOK, http.MethodGet, "/v1/imports/"+queued.Id.String(), key, nil, &got)
	if got.Status != httpapi.Queued || got.Id != queued.Id {
		t.Errorf("GET queued import %+v", got)
	}
	var list []httpapi.ImportRun
	e.must(http.StatusOK, http.MethodGet, "/v1/imports", key, nil, &list)
	if len(list) != 1 || list[0].Id != queued.Id || list[0].Status != httpapi.Queued {
		t.Errorf("GET /v1/imports = %+v", list)
	}
	var gen httpapi.GeneratorRun
	e.must(http.StatusAccepted, http.MethodPost, "/v1/generator-runs", key, nil, &gen)
	e.must(http.StatusOK, http.MethodGet, "/v1/generator-runs/"+gen.Id.String(), key, nil, &gen)
	if gen.Status != httpapi.Queued {
		t.Errorf("GET queued generator run %+v", gen)
	}

	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	e.reader = key
	if run := e.waitImport(queued.Id); run.Status != httpapi.Succeeded {
		t.Errorf("the queued import ran as %+v", run)
	}
}

// Cost estimates come from the newest succeeded generator run, filtered by model, billing and region.
func TestCostEstimates(t *testing.T) {
	e := newEnv(t, "api_costs", false)
	var none []httpapi.CostEstimate
	e.must(http.StatusOK, http.MethodGet, "/v1/cost-estimates", e.reader, nil, &none)
	if none == nil || len(none) != 0 {
		t.Fatalf("cost estimates without runs: %+v", none)
	}
	asOf := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	run := func(started time.Time, status string) uuid.UUID {
		id := uuid.New()
		if err := e.db.Exec(`INSERT INTO generator_runs (id, created_at, updated_at, status, started_at, input_digest, input_import_runs, schema_version, report, error)
			VALUES (?, now(), now(), ?, ?, '', '[]', '4.1.0', '[]', '')`, id, status, started).Error; err != nil {
			t.Fatal(err)
		}
		return id
	}
	estimate := func(run uuid.UUID, model, billing, region string, usd float64) {
		if err := e.db.Exec(`INSERT INTO cost_estimates (id, created_at, updated_at, generator_run_id, model_slug, setup, fit_key, node_type_id, region, billing,
			instances, usd_per_hour, usd_per_month, price_source, price_source_url, price_as_of)
			VALUES (gen_random_uuid(), now(), now(), ?, ?, 'optimal', '', 'azure/Standard_ND96isr_H100_v5', ?, ?, 1, ?, ?, 'azure-retail-prices', 'https://prices.azure.com', ?)`,
			run, model, region, billing, usd, usd*730, asOf).Error; err != nil {
			t.Fatal(err)
		}
	}
	old := run(time.Now().Add(-2*time.Hour), "succeeded")
	estimate(old, "llama-3.1-70b", "on_demand", "eastus", 1)
	current := run(time.Now().Add(-time.Hour), "succeeded")
	estimate(current, "llama-3.1-70b", "on_demand", "eastus", 98.32)
	estimate(current, "llama-3.1-70b", "spot", "eastus", 39.5)
	estimate(current, "llama-3.1-70b", "on_demand", "westeurope", 110)
	estimate(current, "mixtral-8x7b", "on_demand", "eastus", 98.32)
	failed := run(time.Now(), "failed")
	estimate(failed, "llama-3.1-70b", "on_demand", "eastus", 2)

	for query, want := range map[string]int{"": 4, "?model=llama-3.1-70b": 3, "?model=llama-3.1-70b&billing=on_demand": 2,
		"?model=llama-3.1-70b&billing=on_demand&region=eastus": 1, "?region=westeurope": 1, "?model=nope": 0} {
		var got []httpapi.CostEstimate
		e.must(http.StatusOK, http.MethodGet, "/v1/cost-estimates"+query, e.reader, nil, &got)
		if len(got) != want {
			t.Errorf("GET /v1/cost-estimates%s = %d estimates, want %d", query, len(got), want)
		}
		for _, c := range got {
			if c.GeneratorRun != current {
				t.Errorf("estimate %+v is not of the newest succeeded run", c)
			}
		}
	}
	var one []httpapi.CostEstimate
	e.must(http.StatusOK, http.MethodGet, "/v1/cost-estimates?model=llama-3.1-70b&billing=on_demand&region=eastus", e.reader, nil, &one)
	if c := one[0]; c.UsdPerHour != 98.32 || math.Abs(c.UsdPerMonth-98.32*730) > 1e-6 || c.Setup != httpapi.Optimal || !c.PriceAsOf.Equal(asOf) || c.Instances != 1 {
		t.Errorf("estimate %+v", c)
	}
	e.must(http.StatusBadRequest, http.MethodGet, "/v1/cost-estimates?billing=monthly", e.reader, nil, nil)
}

// Generated cost estimates use API-curated prices, carry the price's
// provenance and mark the cheapest setup.
func TestGeneratedCostEstimates(t *testing.T) {
	e := newEnv(t, "api_generated_costs", false)
	e.importBootstrap()
	const mi300x = "azure/Standard_ND96isr_MI300X_v5"
	var prices []httpapi.Price
	e.must(http.StatusOK, http.MethodPut, "/v1/local/prices", e.admin, []httpapi.Price{
		{NodeTypeId: mi300x, Region: "eastus2", Billing: httpapi.OnDemand, UsdPerHour: 48},
		{NodeTypeId: mi300x, Region: "westus3", Billing: httpapi.OnDemand, UsdPerHour: 49},
	}, &prices)
	run := e.generate()
	var got []httpapi.CostEstimate
	e.must(http.StatusOK, http.MethodGet, "/v1/cost-estimates?model=llama-3.1-70b&region=eastus2", e.reader, nil, &got)
	var lines []string
	for _, c := range got {
		if c.GeneratorRun != run.Id || c.PriceSource != "curation" || !c.PriceAsOf.Equal(*prices[0].AsOf) || c.NodeTypeId != mi300x {
			t.Errorf("estimate %+v, want run %s at the local price as of %s", c, run.Id, prices[0].AsOf)
		}
		lines = append(lines, fmt.Sprintf("%s %s %d %v %v %t", c.Setup, c.FitKey, c.Instances, c.UsdPerHour, c.UsdPerMonth, c.Cheapest))
	}
	want := []string{
		"fit llama-3.1-70b/MI300X/-/bf16/auto 1 48 35040 false",
		"fit llama-3.1-70b/MI300X/-/bf16/auto/2-node 2 96 70080 false",
		"fit llama-3.1-70b/MI300X/-/fp8/auto 1 48 35040 false",
		"optimal  1 48 35040 true",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("GET /v1/cost-estimates =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func modelOf(t *testing.T, c httpapi.EffectiveCatalog, slug string) httpapi.ModelTemplate {
	t.Helper()
	for _, m := range c.Models {
		if m.Slug == slug {
			return m
		}
	}
	t.Fatalf("no model %s", slug)
	return httpapi.ModelTemplate{}
}

func nodeTypeOf(t *testing.T, c httpapi.EffectiveCatalog, id string) httpapi.NodeType {
	t.Helper()
	for _, n := range c.NodeTypes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node type %s", id)
	return httpapi.NodeType{}
}

// An unreachable issuer gives 503 with Retry-After, not 401; API keys still work.
func TestAuthUnavailable(t *testing.T) {
	e := newEnv(t, "api_auth_down", false)
	e.issuer.Down.Store(true)
	if code, b := e.call(http.MethodGet, "/v1/catalog", e.admin, nil); code != http.StatusServiceUnavailable || e.header.Get("Retry-After") == "" {
		t.Errorf("GET with the issuer down = %d %s (Retry-After %q), want 503 with Retry-After", code, b, e.header.Get("Retry-After"))
	}
	e.must(http.StatusOK, http.MethodGet, "/v1/catalog", e.operator, nil, nil)
	e.issuer.Down.Store(false)
	e.must(http.StatusOK, http.MethodGet, "/v1/catalog", e.admin, nil, nil)
}
