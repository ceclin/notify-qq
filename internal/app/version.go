package app

import (
	"encoding/json"
	"io"
	"runtime"

	"github.com/samber/oops"
)

// Release builds supply these values through linker flags, so the same command
// identifies native archives and container images without loading credentials.
var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

func writeVersion(output io.Writer) error {
	metadata := struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuiltAt   string `json:"built_at"`
		GoVersion string `json:"go_version"`
	}{version, commit, builtAt, runtime.Version()}
	err := json.NewEncoder(output).Encode(metadata)
	return oops.In("command").Code("command_output_failed").Wrapf(err, "version information could not be written")
}
