//go:build macapp

package macapp

import _ "embed"

//go:embed embedded/AgentTincan.zip
var embeddedZip []byte
