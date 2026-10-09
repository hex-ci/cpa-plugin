// config.go parses the plugin configuration the host pushes on
// register/reconfigure. Unknown keys are ignored; the few keys the plugin owns
// are type-checked so a typo surfaces as a configuration error instead of a
// silently ignored setting.
package main

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

func parseTopLevelConfigScalars(raw []byte) (map[string]string, error) {
	values := map[string]string{}
	if len(raw) == 0 {
		return values, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("invalid plugin configuration: " + err.Error())
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return values, nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return values, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		switch key.Value {
		case "management_key", "management-key", "proxy-url", "proxy_url",
			"panel_base_url", "panel-base-url",
			"login_callback_url", "login-callback-url":
		case "desensitize", "checkin_auto", "checkin-auto":
			// The only boolean key the plugin owns, so the scalar type check
			// below (strings only) has to branch out here.
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return nil, errors.New(key.Value + " must be true or false")
			}
			values[key.Value] = value.Value
			continue
		default:
			continue
		}
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return nil, errors.New(key.Value + " must be a scalar")
		}
		if value.Tag != "!!str" {
			return nil, errors.New(key.Value + " must be a string")
		}
		values[key.Value] = strings.TrimSpace(value.Value)
	}
	return values, nil
}
