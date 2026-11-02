// Package seznamwebmaster holds the assets compiled into the szwm binary, so an
// agent can read its instructions and the API contract with no repository.
package seznamwebmaster

import _ "embed"

// SkillMD is the agent guide printed by `szwm guide`.
//
//go:embed skills/seznam-webmaster/SKILL.md
var SkillMD string

// SwaggerSnapshot is the API contract szwm was built against. `szwm doctor`
// compares it with the live swagger.json to detect upstream changes.
//
//go:embed docs/swagger.snapshot.json
var SwaggerSnapshot []byte
