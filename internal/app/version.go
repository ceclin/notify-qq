package app

import (
	"encoding/json"
	"fmt"
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

func writeVersion(output io.Writer, jsonOutput bool) error {
	metadata := struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuiltAt   string `json:"built_at"`
		GoVersion string `json:"go_version"`
	}{version, commit, builtAt, runtime.Version()}
	var err error
	if jsonOutput {
		err = json.NewEncoder(output).Encode(metadata)
	} else {
		if _, err = fmt.Fprintln(output, "notify-qq", version); err == nil {
			err = writeFields(output,
				[2]string{"Commit", commit},
				[2]string{"Built at", builtAt},
				[2]string{"Go version", runtime.Version()},
			)
		}
	}
	return oops.In("command").Code("command_output_failed").Wrapf(err, "version information could not be written")
}
