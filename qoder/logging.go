// logging.go routes plugin messages into the host log (main.log).
//
// Level discipline: production runs with debug disabled, so anything an
// operator must see has to be info or above. The host's formatter does not
// render structured fields — every value worth reading belongs in the message
// string itself.
package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// hostLogf emits one log line through the host. Failures are ignored: logging
// must never break a request path.
func hostLogf(level, message string) {
	if message == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"level":   level,
		"message": "qoder: " + message,
	})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}
