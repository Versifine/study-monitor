package webassets

import "embed"

// Dist contains the deterministic output of build.mjs. Production serves only
// these committed assets and never needs Node.js or a development server.
//
//go:embed dist/index.html dist/assets/*.css dist/assets/*.js
var Dist embed.FS
