package main

import (
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/agentfeedback/agentfeedback/v4/pkg/collect"
)

// checkConfigKeys reports, as problems, the keys under the [collect],
// [context] and [detect] tables of the config file at path that no setting reads: a
// misspelt narrowing key is otherwise silently not applied. Unknown
// top-level keys are left alone, so an older binary reads a newer file.
func checkConfigKeys(path string, problem func(error)) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, k := range unknownConfigKeys(data) {
		problem(errConfigUnknownKey(path, k))
	}
}

// unknownConfigKeys lists, sorted, the keys under the collect, context and
// detect tables of a config file that are not toml tags of collect.Policy,
// collect.ContextConfig and detectConfig, as table.key.
func unknownConfigKeys(data []byte) []string {
	var doc map[string]any
	if toml.Unmarshal(data, &doc) != nil {
		return nil
	}
	var out []string
	for table, known := range map[string][]string{
		"collect": tomlKeys(reflect.TypeFor[collect.Policy]()),
		"context": tomlKeys(reflect.TypeFor[collect.ContextConfig]()),
		"detect":  tomlKeys(reflect.TypeFor[detectConfig]()),
	} {
		sub, ok := doc[table].(map[string]any)
		if !ok {
			continue
		}
		for k := range sub {
			// go-toml v2 matches field names case-insensitively.
			if !slices.ContainsFunc(known, func(n string) bool { return strings.EqualFold(n, k) }) {
				out = append(out, table+"."+k)
			}
		}
	}
	slices.Sort(out)

	return out
}

// tomlKeys lists the toml tag names of a struct's fields.
func tomlKeys(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("toml"), ","); name != "" && name != "-" {
			out = append(out, name)
		}
	}

	return out
}
