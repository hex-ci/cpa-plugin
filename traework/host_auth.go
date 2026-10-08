// host_auth.go wraps the host's auth-store RPC (host.auth.list / get / save).
// A login the host drives is persisted by the host from the AuthData the plugin
// returns; a login the panel drives has no host session, so the plugin writes
// that same auth file itself through host.auth.save.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

// isTraeworkAuthListName matches the provider's auth files. The file name is the
// only reliable discriminator across host versions, so both the legacy single
// name and the per-account prefix are accepted.
func isTraeworkAuthListName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == authFileName || strings.HasPrefix(name, providerName+"-")
}

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	for _, f := range resp.Files {
		if isTraeworkAuthListName(f.Name) {
			out = append(out, f)
		}
	}
	return out, nil
}

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}

// hostAuthSave writes one auth file through the host. The payload is the auth
// record the host would write for a host-driven login, so a panel-side save and
// a host-side save land on the same path.
func hostAuthSave(name string, payload []byte) (string, error) {
	body, err := json.Marshal(pluginapi.HostAuthSaveRequest{Name: name, JSON: payload})
	if err != nil {
		return "", err
	}
	raw, err := hostCall(pluginabi.MethodHostAuthSave, body)
	if err != nil {
		return "", err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return "", fmt.Errorf("host.auth.save: bad envelope")
	}
	var resp pluginapi.HostAuthSaveResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Name) == "" {
		return "", fmt.Errorf("host.auth.save: host returned no file name")
	}
	return resp.Name, nil
}
