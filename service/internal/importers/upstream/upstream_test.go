// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package upstream_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"gorm.io/gorm"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/dbtest"
	"github.com/fishman/neocloud-catalog/service/internal/generator"
	"github.com/fishman/neocloud-catalog/service/internal/importers"
	"github.com/fishman/neocloud-catalog/service/internal/importers/bootstrap"
	"github.com/fishman/neocloud-catalog/service/internal/importers/upstream"
	"github.com/fishman/neocloud-catalog/service/internal/model"
	"github.com/fishman/neocloud-catalog/service/internal/release"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

const coreDir = "../../../.."

func newStore(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	sqlDB, db := dbtest.Fresh(t, suffix)
	dbtest.Migrate(t, sqlDB)
	return store.New(db), db
}

func ourDeployment(t *testing.T, suffix string) (*store.Store, *gorm.DB) {
	t.Helper()
	s, db := newStore(t, suffix)
	imp, err := bootstrap.New(filepath.Join(coreDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importers.Execute(ctx, s, imp, uuid.New()); err != nil {
		t.Fatal(err)
	}
	return s, db
}

// releaseAt releases s's catalog as generated at version, so later generations
// are newer.
func releaseAt(t *testing.T, s *store.Store, db *gorm.DB, version string) release.Release {
	t.Helper()
	run, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE generator_runs SET snapshot = jsonb_set(snapshot, '{generated_at}', ?::jsonb) WHERE id = ?`,
		strconv.Quote(version), run.ID).Error; err != nil {
		t.Fatal(err)
	}
	rel, err := release.Create(ctx, s, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != version {
		t.Fatalf("release version %s, want %s", rel.Version, version)
	}
	return rel
}

func publish(t *testing.T, rel release.Release, target release.Target) {
	t.Helper()
	if err := release.Publish(ctx, rel, target); err != nil {
		t.Fatal(err)
	}
}

func writeLayer(t *testing.T, s *store.Store, layer string, in store.Inputs) {
	t.Helper()
	run, err := s.StartImport(ctx, uuid.New(), "test", layer, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ReplaceLayer(ctx, layer, run.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishImport(ctx, run.ID, "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func effective(t *testing.T, s *store.Store) store.Inputs {
	t.Helper()
	in, err := s.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func sameCore(t *testing.T, got, want store.Inputs) {
	t.Helper()
	enc := func(in store.Inputs) string {
		b, err := json.Marshal([]any{in.Families, in.NodeTypes, in.Models, in.Drafts, in.Backends})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if g, w := enc(got), enc(want); g != w {
		t.Errorf("core objects differ:\n got %.600s\nwant %.600s", g, w)
	}
}

func clone[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var c T
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func runImport(t *testing.T, s *store.Store, imp importers.Importer) (model.ImportRun, importers.Summary) {
	t.Helper()
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var sum importers.Summary
	if err := json.Unmarshal(run.Summary, &sum); err != nil {
		t.Fatal(err)
	}
	return run, sum
}

// retag points tag at artifact, as a registry replaying an older release does.
func retag(t *testing.T, tag, artifact string) {
	t.Helper()
	d, err := name.NewDigest(artifact)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(d)
	if err != nil {
		t.Fatal(err)
	}
	tg, err := name.NewTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Tag(tg, desc); err != nil {
		t.Fatal(err)
	}
}

func payload(tier string) catalog.Extensions {
	return catalog.Extensions{"example.com/tier": json.RawMessage(`{"tier":"` + tier + `"}`)}
}

// tier decodes the payload; jsonb and encoding/json each re-space it.
func tier(t *testing.T, ext catalog.Extensions) string {
	t.Helper()
	var p struct {
		Tier string `json:"tier"`
	}
	if raw, ok := ext["example.com/tier"]; ok {
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
	}
	return p.Tier
}

// newModel is m as slug without its speculative presets.
func newModel(t *testing.T, m catalog.ModelTemplate, slug string) catalog.ModelTemplate {
	t.Helper()
	c := clone(t, m)
	c.Slug = slug
	c.Presets = slices.DeleteFunc(c.Presets, func(p catalog.Preset) bool { return p.Speculative != nil })
	return c
}

// The import verifies our release and replaces the upstream layer; local stays;
// no overlay empties the overlay layer.
func TestUpstreamImportReplacesUpstreamLayer(t *testing.T) {
	ours, ourDB := ourDeployment(t, "upstream_replace_ours")
	rel := releaseAt(t, ours, ourDB, "2026-01-01T00:00:00Z")
	signer, pub := keyPair(t)
	ref := registryHost(t) + "/neocloud/catalog:v4"
	publish(t, rel, release.Target{Ref: ref, Signer: signer})
	want := effective(t, ours)

	s, _ := newStore(t, "upstream_replace_operator")
	stale := newModel(t, want.Models[0], "stale-model")
	writeLayer(t, s, model.LayerUpstream, store.Inputs{Models: []catalog.ModelTemplate{stale}})
	local := newModel(t, want.Models[0], "operator-model")
	if err := s.Upsert(ctx, local); err != nil {
		t.Fatal(err)
	}
	writeLayer(t, s, model.LayerOverlay, store.Inputs{Extensions: catalog.Overlay{Models: map[string]catalog.Extensions{local.Slug: payload("gold")}}})

	imp := upstream.New(newSource(t, upstream.Config{Ref: ref, PublicKey: pub}), nil)
	if o := imp.Origin(); imp.Name() != "upstream" || o.Layer != model.LayerUpstream || o.Source != "upstream" || o.SourceURL != ref {
		t.Errorf("importer %s writes %+v, want the upstream layer from source upstream at %s", imp.Name(), o, ref)
	}
	run, sum := runImport(t, s, imp)
	if run.InputVersion != rel.Version+" "+rel.Digest {
		t.Errorf("input version %q, want the release version and digest %q", run.InputVersion, rel.Version+" "+rel.Digest)
	}
	if sum.Sources == nil || *sum.Sources != (importers.SourceCounts{Read: 1}) || sum.Changes["models"].Deleted != 1 ||
		sum.Changes["extension_payloads"].Deleted != 1 {
		t.Errorf("summary = %+v, want one source read, the stale model and the overlay payload deleted", sum)
	}

	got := effective(t, s)
	last := len(got.Models) - 1
	if got.Models[last].Slug != local.Slug {
		t.Errorf("last effective model %s, want the local %s", got.Models[last].Slug, local.Slug)
	}
	if len(got.Extensions.Models) != 0 {
		t.Errorf("overlay layer = %+v, want it emptied", got.Extensions)
	}
	got.Models = got.Models[:last]
	sameCore(t, got, want)
	snap, _, err := catalog.Generate(got.Families, got.NodeTypes, got.Models, got.Drafts, got.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version = rel.Version; snap.Digest() != rel.Digest {
		t.Errorf("the imported catalog generates digest %s, want our release's %s", snap.Digest(), rel.Digest)
	}
	p := dbtest.Provenance(t, s, store.KindModel, want.Models[0].Slug)
	if len(p) != 1 || p[0].Layer != model.LayerUpstream || p[0].Source != "upstream" || p[0].SourceURL != ref ||
		p[0].ImportRunID == nil || *p[0].ImportRunID != run.ID {
		t.Errorf("provenance of %s = %+v, want the upstream row of run %s from %s", want.Models[0].Slug, p, run.ID, ref)
	}
	if p := dbtest.Provenance(t, s, store.KindModel, local.Slug); len(p) != 1 || p[0].Source != model.SourceCuration {
		t.Errorf("provenance of %s = %+v, want the curated local row", local.Slug, p)
	}

	before := effective(t, s)
	again, sum := runImport(t, s, imp)
	if again.InputVersion != run.InputVersion || len(sum.Changes) != 0 {
		t.Errorf("second run: input version %q, changes %+v; want %q and none", again.InputVersion, sum.Changes, run.InputVersion)
	}
	if after := effective(t, s); !reflect.DeepEqual(after, before) {
		t.Error("a second run of the same release changed the effective catalog")
	}
}

// A replayed older signed release fails the run and leaves the layer.
func TestUpstreamImportRefusesOlderRelease(t *testing.T) {
	ours, ourDB := ourDeployment(t, "upstream_older_ours")
	older := releaseAt(t, ours, ourDB, "2026-01-01T00:00:00Z")
	extra := newModel(t, effective(t, ours).Models[0], "newer-model")
	if err := ours.Upsert(ctx, extra); err != nil {
		t.Fatal(err)
	}
	newer := releaseAt(t, ours, ourDB, "2026-02-01T00:00:00Z")
	signer, pub := keyPair(t)
	ref := registryHost(t) + "/neocloud/catalog:v4"
	publish(t, older, release.Target{Ref: ref, Signer: signer})
	publish(t, newer, release.Target{Ref: ref, Signer: signer})
	artifact := func(rel release.Release) string {
		got, err := release.Get(ctx, ours, rel.Version)
		if err != nil {
			t.Fatal(err)
		}
		return got.ArtifactRef
	}

	s, _ := newStore(t, "upstream_older_operator")
	imp := upstream.New(newSource(t, upstream.Config{Ref: ref, PublicKey: pub}), nil)
	if run, _ := runImport(t, s, imp); !strings.HasPrefix(run.InputVersion, newer.Version+" ") {
		t.Fatalf("imported %q, want %s", run.InputVersion, newer.Version)
	}
	before := effective(t, s)

	retag(t, ref, artifact(older))
	run, err := importers.Execute(ctx, s, imp, uuid.New())
	if err == nil || !strings.Contains(err.Error(), "older than "+newer.Version) || run.Status != model.RunFailed {
		t.Fatalf("import of the replayed release: %s, %v; want a failed run refusing the older version", run.Status, err)
	}
	if after := effective(t, s); !reflect.DeepEqual(after, before) {
		t.Error("the refused release changed the effective catalog")
	}

	retag(t, ref, artifact(newer))
	if _, sum := runImport(t, s, imp); len(sum.Changes) != 0 {
		t.Errorf("re-importing the release imported last changed %+v", sum.Changes)
	}
}

// An overlay imports only pinned to the release and signed with its own key.
func TestOverlayImportRequiresPin(t *testing.T) {
	ours, ourDB := ourDeployment(t, "upstream_pin_ours")
	m := effective(t, ours).Models[0]
	writeLayer(t, ours, model.LayerOverlay, store.Inputs{Extensions: catalog.Overlay{Models: map[string]catalog.Extensions{m.Slug: payload("gold")}}})
	relA := releaseAt(t, ours, ourDB, "2026-01-01T00:00:00Z")
	signer, pub := keyPair(t)
	host := registryHost(t)
	catRef, ovRef := host+"/neocloud/catalog:v4", host+"/enterprise/overlay:v4"
	publish(t, relA, release.Target{Ref: catRef, OverlayRef: ovRef, Signer: signer})

	s, db := newStore(t, "upstream_pin_operator")
	imp := upstream.New(newSource(t, upstream.Config{Ref: catRef, PublicKey: pub}),
		newSource(t, upstream.Config{Ref: ovRef, PublicKey: pub, Overlay: true}))
	run, sum := runImport(t, s, imp)
	ovTag, err := name.ParseReference(ovRef)
	if err != nil {
		t.Fatal(err)
	}
	ovDigest, err := remote.Head(ovTag)
	if err != nil {
		t.Fatal(err)
	}
	if want := relA.Version + " " + relA.Digest + " overlay " + ovDigest.Digest.String(); run.InputVersion != want || sum.Sources.Read != 2 {
		t.Errorf("input version %q, %d sources read; want %q, 2", run.InputVersion, sum.Sources.Read, want)
	}
	pinnedA := effective(t, s)
	if got := pinnedA.Extensions.Models[m.Slug]; tier(t, got) != "gold" {
		t.Errorf("overlay layer payload of %s = %s, want our overlay's", m.Slug, got)
	}
	var row model.ExtensionPayload
	if err := db.Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Layer != model.LayerOverlay || row.Source != "upstream" || row.SourceURL != ovRef || row.ImportRunID == nil || *row.ImportRunID != run.ID {
		t.Errorf("overlay payload provenance = %+v, want run %s from %s", row.Provenance, run.ID, ovRef)
	}

	refused := func(what, wantErr string) {
		t.Helper()
		run, err := importers.Execute(ctx, s, imp, uuid.New())
		if err == nil || !strings.Contains(err.Error(), wantErr) || run.Status != model.RunFailed {
			t.Errorf("%s: %s, %v; want a failed run (%s)", what, run.Status, err, wantErr)
		}
		got := effective(t, s)
		sameCore(t, got, pinnedA)
		if !reflect.DeepEqual(got.Extensions, pinnedA.Extensions) {
			t.Errorf("%s changed the overlay layer", what)
		}
	}
	// The next release ships without an overlay; the tag still names the previous one.
	extra := newModel(t, m, "newer-model")
	if err := ours.Upsert(ctx, extra); err != nil {
		t.Fatal(err)
	}
	relB := releaseAt(t, ours, ourDB, "2026-02-01T00:00:00Z")
	publish(t, relB, release.Target{Ref: catRef, Signer: signer})
	refused("an overlay pinned to the previous release", "overlay for snapshot digest "+relA.Digest+", not "+relB.Digest)

	stranger, _ := keyPair(t)
	d := pushOverlay(t, ovRef, relB.Overlay)
	sign(t, stranger, host+"/enterprise/overlay", d)
	refused("the new overlay signed by another key", "overlay signature")

	sign(t, signer, host+"/enterprise/overlay", d)
	runImport(t, s, imp)
	got := effective(t, s)
	if got.Models[len(got.Models)-1].Slug != extra.Slug || tier(t, got.Extensions.Models[m.Slug]) != "gold" {
		t.Error("the release with its pinned overlay did not import")
	}
}

func extensions(snap catalog.CatalogSnapshot) int {
	n := 0
	for _, f := range snap.Families {
		n += len(f.Extensions)
	}
	for _, m := range snap.Models {
		n += len(m.Extensions)
		for _, p := range m.Presets {
			n += len(p.Extensions)
		}
	}
	for _, f := range snap.Fits {
		n += len(f.Extensions)
	}
	for _, b := range snap.Backends {
		for _, v := range b.Versions {
			n += len(v.Extensions)
		}
	}
	return n
}

// An operator's re-exported release and re-pinned overlay verify and sync in
// neocloud-api.
func TestOperatorReexportSyncsInAPI(t *testing.T) {
	ours, ourDB := ourDeployment(t, "reexport_ours")
	m := effective(t, ours).Models[0]
	ourOverlay := catalog.Overlay{Models: map[string]catalog.Extensions{m.Slug: payload("gold")},
		Presets: map[string]catalog.Extensions{m.Slug + "/" + m.Presets[0].Name: payload("silver")}}
	writeLayer(t, ours, model.LayerOverlay, store.Inputs{Extensions: ourOverlay})
	ourRelease := releaseAt(t, ours, ourDB, "2026-01-01T00:00:00Z")
	ourSigner, ourPub := keyPair(t)
	host := registryHost(t)
	ourRef, ourOverlayRef := host+"/neocloud/catalog:v4", host+"/neocloud/overlay:v4"
	publish(t, ourRelease, release.Target{Ref: ourRef, OverlayRef: ourOverlayRef, Signer: ourSigner})

	s, _ := newStore(t, "reexport_operator")
	runImport(t, s, upstream.New(newSource(t, upstream.Config{Ref: ourRef, PublicKey: ourPub}),
		newSource(t, upstream.Config{Ref: ourOverlayRef, PublicKey: ourPub, Overlay: true})))
	local := newModel(t, m, "operator-model")
	if err := s.Upsert(ctx, local); err != nil {
		t.Fatal(err)
	}
	gen, err := generator.Run(ctx, s, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := release.Create(ctx, s, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	opSigner, opPub := keyPair(t)
	opRef, opOverlayRef := host+"/operator/catalog:v4", host+"/operator/overlay:v4"
	publish(t, theirs, release.Target{Ref: opRef, OverlayRef: opOverlayRef, Signer: opSigner})

	api := newSource(t, upstream.Config{Ref: opRef, PublicKey: opPub})
	apiOverlay := newSource(t, upstream.Config{Ref: opOverlayRef, PublicKey: opPub, Overlay: true})
	rel, err := api.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ourTime, err := catalog.ParseVersion(ourRelease.Version)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != theirs.Version || !rel.Time.After(ourTime) {
		t.Errorf("operator version %s, want %s, newer than ours %s", rel.Version, theirs.Version, ourRelease.Version)
	}
	snap, err := api.Pull(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaVersion != catalog.SchemaVersion {
		t.Errorf("snapshot schema %s, want %s", snap.SchemaVersion, catalog.SchemaVersion)
	}
	ov, err := apiOverlay.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, err := apiOverlay.PullOverlay(ctx, ov)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Digest() != theirs.Digest || o.SnapshotDigest != theirs.Digest {
		t.Errorf("export digest %s, overlay pinned to %s; want both the operator release's %s", snap.Digest(), o.SnapshotDigest, theirs.Digest)
	}
	merged, err := catalog.Merge(snap, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := extensions(snap); n != 0 {
		t.Errorf("the operator's export carries %d extension payloads", n)
	}
	if got := extensions(merged); got != 2 || tier(t, merged.Models[0].Extensions) != "gold" {
		t.Errorf("merged %d payloads (model %s: %s), want our overlay's 2", got, merged.Models[0].Slug, merged.Models[0].Extensions)
	}
	if !slices.ContainsFunc(snap.Models, func(x catalog.ModelTemplate) bool { return x.Slug == local.Slug }) {
		t.Error("the operator's export lacks its local model")
	}

	for _, cfg := range []upstream.Config{{Ref: opRef, PublicKey: ourPub}, {Ref: opOverlayRef, PublicKey: ourPub, Overlay: true}} {
		if _, err := newSource(t, cfg).Check(ctx); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Errorf("%s verified with our key: %v", cfg.Ref, err)
		}
	}
}
