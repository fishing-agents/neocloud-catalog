// Atlas migration env: the GORM models (internal/model) are the schema
// authority; this env diffs versioned migrations from them. Run
// `atlas migrate diff <name> --env gorm` (make migrate-diff NAME=<name>).
data "external_schema" "gorm" {
  program = [
    "go", "run", "-tags", "atlas", "./tools/atlas"
  ]
}

env "gorm" {
  src = data.external_schema.gorm.url
  dev = "docker://postgres/16/dev?search_path=public"
  migration {
    dir = "file://migrations"
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}
