// Package model holds the catalog service's GORM models, the schema authority
// Atlas diffs versioned migrations from. Curated and imported objects refer to
// each other by natural key (slug, name, version), never by row id, so a local
// object overrides or extends an upstream one per key; only the parts of one
// object (a source's files) and derived rows point at rows.
package model

import (
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	catalog "github.com/fishman/neocloud-catalog"
)

// Layers: local overrides upstream per natural key; overlay only adds
// extension payloads.
const (
	LayerUpstream = "upstream"
	LayerLocal    = "local"
	LayerOverlay  = "overlay"
)

// Billing types of a node type price.
const (
	BillingOnDemand   = "on_demand"
	BillingSpot       = "spot"
	BillingReserved1Y = "reserved_1y"
)

// Run statuses of import and generator runs.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// Timestamps is the created/updated audit pair shared by every row.
type Timestamps struct {
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Base is the UUID identity. Rows are deleted for real: a soft-deleted row
// would keep holding its natural key and block curating that key again.
type Base struct {
	ID uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
}

// Provenance records where a curated or imported row came from. Layer leads
// each table's natural key, the unique index idx_<table>_natural. ImportRunID
// is nil for curation.
type Provenance struct {
	Layer       string     `gorm:"not null;uniqueIndex:,composite:natural,priority:1;check:layer IN ('upstream', 'local', 'overlay')"`
	Source      string     `gorm:"not null"`
	SourceURL   string     `gorm:"column:source_url;not null"`
	AsOf        time.Time  `gorm:"not null"`
	ImportRunID *uuid.UUID `gorm:"type:uuid;index"`
	ImportRun   *ImportRun `gorm:"foreignKey:ImportRunID"`
}

// Derivation ties a derived row to the generator run that wrote it; deleting
// the run deletes its rows. The run leads each table's key, idx_<table>_key.
type Derivation struct {
	GeneratorRunID uuid.UUID     `gorm:"type:uuid;not null;uniqueIndex:,composite:key,priority:1"`
	GeneratorRun   *GeneratorRun `gorm:"foreignKey:GeneratorRunID;constraint:OnDelete:CASCADE"`
}

// SourceCuration is the source of a hand-curated value.
const SourceCuration = "curation"

// FieldSource is where one field's value came from.
type FieldSource struct {
	Source      string     `json:"source"`
	SourceURL   string     `json:"source_url,omitempty"`
	AsOf        time.Time  `json:"as_of"`
	ImportRunID *uuid.UUID `json:"import_run_id,omitempty"`
}

// FieldProvenance maps a field name to where that field's value came from;
// the row's Provenance stays the row's origin.
type FieldProvenance map[string]FieldSource

// CuratedFields returns the fields whose value is curated, sorted; importers
// leave them alone.
func (p FieldProvenance) CuratedFields() []string {
	var fields []string
	for f, s := range p {
		if s.Source == SourceCuration {
			fields = append(fields, f)
		}
	}
	slices.Sort(fields)
	return fields
}

type Family struct {
	Base
	Timestamps
	Provenance
	Name               string                                 `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Position           int                                    `gorm:"not null"`
	FieldProvenance    datatypes.JSONType[FieldProvenance]    `gorm:"not null;default:'{}'"`
	Vendor             string                                 `gorm:"not null"`
	Architecture       string                                 `gorm:"not null"`
	ComputeCapability  string                                 `gorm:"not null"`
	DTypes             datatypes.JSONSlice[string]            `gorm:"column:dtypes;not null"`
	MemoryGB           float64                                `gorm:"column:memory_gb;not null"`
	MemoryBandwidthGBs float64                                `gorm:"column:memory_bandwidth_gbs;not null"`
	TFLOPS             datatypes.JSONType[map[string]float64] `gorm:"column:tflops;not null"`
	Interconnect       string                                 `gorm:"not null"`
	InterconnectGBs    float64                                `gorm:"column:interconnect_gbs;not null"`
	Sources            datatypes.JSONSlice[string]            `gorm:"not null"`
}

type NodeType struct {
	Base
	Timestamps
	Provenance
	NodeTypeID      string                              `gorm:"column:node_type_id;not null;uniqueIndex:,composite:natural,priority:2"`
	FieldProvenance datatypes.JSONType[FieldProvenance] `gorm:"not null;default:'{}'"`
	Family          string                              `gorm:"not null;index"`
	GPUs            int                                 `gorm:"column:gpus;not null"`
	// DocURL is the exported catalog.NodeType.Source, the page documenting
	// the node type.
	DocURL string `gorm:"column:doc_url;not null"`
}

// NodeTypePrice is internal: no price leaves the service in an export.
type NodeTypePrice struct {
	Base
	Timestamps
	Provenance
	NodeTypeID string  `gorm:"column:node_type_id;not null;uniqueIndex:,composite:natural,priority:2"`
	Region     string  `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Billing    string  `gorm:"not null;uniqueIndex:,composite:natural,priority:4;check:billing IN ('on_demand', 'spot', 'reserved_1y')"`
	USDPerHour float64 `gorm:"column:usd_per_hour;type:numeric;not null"`
}

// Model keeps ReleaseDate as the curated string so exports round-trip it.
type Model struct {
	Base
	Timestamps
	Provenance
	Slug            string                                   `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Position        int                                      `gorm:"not null"`
	FieldProvenance datatypes.JSONType[FieldProvenance]      `gorm:"not null;default:'{}'"`
	Name            string                                   `gorm:"not null"`
	Family          string                                   `gorm:"not null"`
	Task            string                                   `gorm:"not null"`
	Capabilities    datatypes.JSONSlice[string]              `gorm:"not null"`
	Description     string                                   `gorm:"not null"`
	ParamsB         float64                                  `gorm:"column:params_b;not null"`
	ParamsActiveB   float64                                  `gorm:"column:params_active_b;not null"`
	ReleaseDate     string                                   `gorm:"not null"`
	Home            string                                   `gorm:"not null"`
	Icon            string                                   `gorm:"not null"`
	Aliases         datatypes.JSONSlice[string]              `gorm:"not null"`
	License         string                                   `gorm:"not null"`
	Gated           bool                                     `gorm:"not null"`
	Architecture    datatypes.JSONType[catalog.Architecture] `gorm:"not null"`
	DefaultContext  int                                      `gorm:"not null"`
	ContextMax      int                                      `gorm:"not null"`
}

// ModelQuantization is one checkpoint of a model; Position keeps the curated
// order.
type ModelQuantization struct {
	Base
	Timestamps
	Provenance
	ModelSlug       string                              `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Name            string                              `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Position        int                                 `gorm:"not null"`
	FieldProvenance datatypes.JSONType[FieldProvenance] `gorm:"not null;default:'{}'"`
	Kind            string                              `gorm:"not null"`
	RequiresDType   string                              `gorm:"column:requires_dtype;not null"`
	WeightsGB       float64                             `gorm:"column:weights_gb;not null"`
}

// ModelSource locates the weights of a quantization (ModelSlug, Quantization)
// or of a draft model (DraftSlug); exactly one owner is set. Preference orders
// an owner's sources.
type ModelSource struct {
	Base
	Timestamps
	Provenance
	ModelSlug    string `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Quantization string `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	DraftSlug    string `gorm:"not null;uniqueIndex:,composite:natural,priority:4;check:chk_model_sources_owner,(draft_slug = '' AND model_slug <> '' AND quantization <> '') OR (draft_slug <> '' AND model_slug = '' AND quantization = '')"`
	Type         string `gorm:"not null;uniqueIndex:,composite:natural,priority:5"`
	Repo         string `gorm:"not null;uniqueIndex:,composite:natural,priority:6"`
	Revision     string `gorm:"not null"`
	Preference   int    `gorm:"not null"`
}

// ModelFile is one entry of a source's cache manifest at its revision; Digest
// is "sha256:<hex>" or "git-oid:<hex>" as in the core.
type ModelFile struct {
	Base
	Timestamps
	Provenance
	ModelSourceID uuid.UUID    `gorm:"type:uuid;not null;index;uniqueIndex:,composite:natural,priority:2"`
	ModelSource   *ModelSource `gorm:"foreignKey:ModelSourceID;constraint:OnDelete:CASCADE"`
	Path          string       `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Position      int          `gorm:"not null"`
	SizeBytes     int64        `gorm:"not null"`
	Digest        string       `gorm:"not null"`
}

type Backend struct {
	Base
	Timestamps
	Provenance
	Name     string `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Position int    `gorm:"not null"`
}

// BackendVersion's flag table lives in flag_tables under the same key.
type BackendVersion struct {
	Base
	Timestamps
	Provenance
	Backend              string                                              `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Version              string                                              `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Position             int                                                 `gorm:"not null"`
	Images               datatypes.JSONType[map[string]string]               `gorm:"not null"`
	Command              datatypes.JSONSlice[string]                         `gorm:"not null"`
	HealthPath           string                                              `gorm:"not null"`
	MinComputeCapability string                                              `gorm:"not null"`
	Quantizations        datatypes.JSONType[map[string]catalog.QuantSupport] `gorm:"not null"`
	Defaults             datatypes.JSONType[map[string]string]               `gorm:"not null"`
}

type FlagTable struct {
	Base
	Timestamps
	Provenance
	Backend        string                                `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	BackendVersion string                                `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Flags          datatypes.JSONType[catalog.FlagTable] `gorm:"not null"`
}

type Preset struct {
	Base
	Timestamps
	Provenance
	ModelSlug         string `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Name              string `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Position          int    `gorm:"not null"`
	Quantization      string `gorm:"not null"`
	Backend           string `gorm:"not null"`
	BackendVersion    string `gorm:"not null"`
	MinBackendVersion string `gorm:"not null"`
	Purpose           string `gorm:"not null"`
	Context           int    `gorm:"not null"`
	TensorParallel    int    `gorm:"not null"`
	KVCacheDType      string `gorm:"column:kv_cache_dtype;not null"`
	PrefixCaching     *bool
	ToolParser        string                                   `gorm:"not null"`
	ReasoningParser   string                                   `gorm:"not null"`
	ChatTemplate      string                                   `gorm:"not null"`
	Speculative       datatypes.JSONType[*catalog.Speculative] `gorm:"not null"`
	Topology          datatypes.JSONType[catalog.Topology]     `gorm:"not null"`
	Env               datatypes.JSONType[map[string]string]    `gorm:"not null"`
	ExtraArgs         datatypes.JSONSlice[string]              `gorm:"not null"`
	Vendors           datatypes.JSONSlice[string]              `gorm:"not null"`
}

// DraftModel's weights are the model_sources row with its DraftSlug.
type DraftModel struct {
	Base
	Timestamps
	Provenance
	Slug            string                              `gorm:"not null;uniqueIndex:,composite:natural,priority:2"`
	Position        int                                 `gorm:"not null"`
	FieldProvenance datatypes.JSONType[FieldProvenance] `gorm:"not null;default:'{}'"`
	Method          string                              `gorm:"not null"`
	Targets         datatypes.JSONSlice[string]         `gorm:"not null"`
	WeightsGB       float64                             `gorm:"column:weights_gb;not null"`
	Backends        datatypes.JSONSlice[string]         `gorm:"not null"`
}

// Fit is keyed by catalog.FitKey: Profile is NULL for whole GPUs, which a
// unique index over the columns would not compare.
type Fit struct {
	Base
	Timestamps
	Derivation
	FitKey         string `gorm:"not null;uniqueIndex:,composite:key,priority:2"`
	ModelSlug      string `gorm:"not null;index"`
	DeviceFamily   string `gorm:"not null"`
	Profile        *string
	Quantization   string  `gorm:"not null"`
	KVCacheDType   string  `gorm:"column:kv_cache_dtype;not null"`
	Mode           string  `gorm:"not null"`
	GPUCount       int     `gorm:"column:gpu_count;not null"`
	Nodes          int     `gorm:"not null"`
	MaxContext     int     `gorm:"not null"`
	VRAMRequiredGB float64 `gorm:"column:vram_required_gb;not null"`
}

// FitEstimate is a fit's performance figure per source ("estimate" from the
// generator).
type FitEstimate struct {
	Base
	Timestamps
	Derivation
	FitID                 uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:,composite:key,priority:2"`
	Fit                   *Fit      `gorm:"foreignKey:FitID;constraint:OnDelete:CASCADE"`
	Source                string    `gorm:"not null;uniqueIndex:,composite:key,priority:3"`
	DecodeTokensPerSec    float64   `gorm:"not null"`
	ConcurrentSequences   int       `gorm:"not null"`
	AggregateTokensPerSec float64   `gorm:"not null"`
	Context               int       `gorm:"not null"`
}

// Guidance is a model's minimum or optimal recommended setup.
type Guidance struct {
	Base
	Timestamps
	Derivation
	ModelSlug             string                      `gorm:"not null;uniqueIndex:,composite:key,priority:2"`
	Kind                  string                      `gorm:"not null;uniqueIndex:,composite:key,priority:3;check:kind IN ('minimum', 'optimal')"`
	Family                string                      `gorm:"not null"`
	GPUs                  int                         `gorm:"column:gpus;not null"`
	Nodes                 int                         `gorm:"not null"`
	Quantization          string                      `gorm:"not null"`
	Preset                string                      `gorm:"not null"`
	Context               int                         `gorm:"not null"`
	NodeTypes             datatypes.JSONSlice[string] `gorm:"not null"`
	DecodeTokensPerSec    float64                     `gorm:"not null"`
	AggregateTokensPerSec float64                     `gorm:"not null"`
	Source                string                      `gorm:"not null"`
	Confidence            string                      `gorm:"not null"`
	FitKeys               datatypes.JSONSlice[string] `gorm:"not null"`
}

func (Guidance) TableName() string { return "guidance" }

// CostEstimate is internal: what a fit (Setup "fit", FitKey set) or a
// guidance setup ("minimum" or "optimal", FitKey empty) costs on a cloud node
// type in a region at a billing type, with the price's provenance. Cheapest
// marks a guidance setup's cheapest estimate per billing type.
type CostEstimate struct {
	Base
	Timestamps
	Derivation
	ModelSlug      string    `gorm:"not null;uniqueIndex:,composite:key,priority:2"`
	Setup          string    `gorm:"not null;uniqueIndex:,composite:key,priority:3;check:setup IN ('fit', 'minimum', 'optimal')"`
	FitKey         string    `gorm:"not null;uniqueIndex:,composite:key,priority:4"`
	NodeTypeID     string    `gorm:"column:node_type_id;not null;uniqueIndex:,composite:key,priority:5"`
	Region         string    `gorm:"not null;uniqueIndex:,composite:key,priority:6"`
	Billing        string    `gorm:"not null;uniqueIndex:,composite:key,priority:7;check:billing IN ('on_demand', 'spot', 'reserved_1y')"`
	Instances      int       `gorm:"not null"`
	USDPerHour     float64   `gorm:"column:usd_per_hour;type:numeric;not null"`
	USDPerMonth    float64   `gorm:"column:usd_per_month;type:numeric;not null"`
	PriceSource    string    `gorm:"not null"`
	PriceSourceURL string    `gorm:"column:price_source_url;not null"`
	PriceAsOf      time.Time `gorm:"not null"`
	Cheapest       bool      `gorm:"not null;default:false"`
}

// ExtensionPayload lives in the overlay layer only. ObjectKind names the
// catalog.Overlay map it belongs to; ObjectKey is that map's key.
type ExtensionPayload struct {
	Base
	Timestamps
	Provenance
	ObjectKind string         `gorm:"not null;uniqueIndex:,composite:natural,priority:2;check:object_kind IN ('families', 'models', 'presets', 'fits', 'backends')"`
	ObjectKey  string         `gorm:"not null;uniqueIndex:,composite:natural,priority:3"`
	Namespace  string         `gorm:"not null;uniqueIndex:,composite:natural,priority:4"`
	Payload    datatypes.JSON `gorm:"type:jsonb;not null;check:chk_extension_payloads_overlay,layer = 'overlay'"`
}

// Release freezes a generator run; Version is its generation time in
// catalog.VersionLayout and Digest the digest of the run's snapshot at that
// version. Overlay holds the overlay layer's payloads as of the release,
// pinned to Digest. ArtifactRef and PublishedAt are set on publish, with
// OverlayRef when its overlay is published too; a published release's
// overlay may be published once later. A database trigger refuses any other
// change, and any delete once published.
type Release struct {
	Base
	Timestamps
	Version        string          `gorm:"not null;uniqueIndex"`
	Digest         string          `gorm:"not null"`
	SchemaVersion  string          `gorm:"not null"`
	ArtifactRef    string          `gorm:"not null"`
	OverlayRef     string          `gorm:"not null;default:''"`
	GeneratorRunID uuid.UUID       `gorm:"type:uuid;not null;index"`
	GeneratorRun   *GeneratorRun   `gorm:"foreignKey:GeneratorRunID"`
	Overlay        catalog.Overlay `gorm:"type:jsonb;serializer:json;not null"`
	PublishedAt    *time.Time
}

// ImportRun is one importer run; importers are idempotent per (Importer,
// InputVersion). Source and SourceURL are the provenance of the rows the run
// writes (an importer's name, or "curation" for curated input), as of StartedAt. Summary holds
// the result and diff summary. ReleaseVersion is the upstream release an
// upstream import wrote, recorded in the transaction that wrote it.
type ImportRun struct {
	Base
	Timestamps
	Importer       string    `gorm:"not null;index:idx_import_runs_input,priority:1"`
	InputVersion   string    `gorm:"not null;index:idx_import_runs_input,priority:2"`
	Source         string    `gorm:"not null"`
	SourceURL      string    `gorm:"column:source_url;not null"`
	Layer          string    `gorm:"not null;check:layer IN ('upstream', 'local', 'overlay')"`
	Status         string    `gorm:"not null;check:status IN ('running', 'succeeded', 'failed')"`
	StartedAt      time.Time `gorm:"not null"`
	FinishedAt     *time.Time
	ReleaseVersion *time.Time
	Summary        datatypes.JSON `gorm:"type:jsonb"`
	Error          string         `gorm:"not null"`
}

// GeneratorRun is one generation over the effective catalog; InputDigest
// identifies the inputs it read, InputImportRuns the import runs that wrote
// them, and Report lists what it reported. Snapshot is the public snapshot a
// succeeded run generated, its version left to the release that freezes it.
type GeneratorRun struct {
	Base
	Timestamps
	Status          string    `gorm:"not null;check:status IN ('running', 'succeeded', 'failed')"`
	StartedAt       time.Time `gorm:"not null"`
	FinishedAt      *time.Time
	InputDigest     string                         `gorm:"not null"`
	InputImportRuns datatypes.JSONSlice[uuid.UUID] `gorm:"not null"`
	SchemaVersion   string                         `gorm:"not null"`
	Report          datatypes.JSONSlice[string]    `gorm:"not null"`
	Snapshot        *catalog.CatalogSnapshot       `gorm:"type:jsonb;serializer:json"`
	Error           string                         `gorm:"not null"`
}

// APIKey is an operator API key, as neocloud-api keeps them: only the SHA-256
// of the key is stored; Prefix is the label in neocatalog_<prefix>_<secret>.
// A key past ExpiresAt no longer authenticates.
type APIKey struct {
	Base
	Timestamps
	Name      string `gorm:"not null"`
	Prefix    string `gorm:"not null"`
	KeyHash   string `gorm:"not null;uniqueIndex"`
	ExpiresAt *time.Time
}

// All lists every model, referenced tables first, for the schema dump and
// AutoMigrate.
func All() []any {
	return []any{
		&ImportRun{},
		&GeneratorRun{},
		&Family{},
		&NodeType{},
		&NodeTypePrice{},
		&Model{},
		&ModelQuantization{},
		&ModelSource{},
		&ModelFile{},
		&Backend{},
		&BackendVersion{},
		&FlagTable{},
		&Preset{},
		&DraftModel{},
		&Fit{},
		&FitEstimate{},
		&Guidance{},
		&CostEstimate{},
		&ExtensionPayload{},
		&Release{},
		&APIKey{},
	}
}
