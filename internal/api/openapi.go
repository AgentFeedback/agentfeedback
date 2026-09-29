package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agentfeedback/agentfeedback/docs"
	"github.com/agentfeedback/agentfeedback/schemas"
)

// openAPIDoc is docs/openapi.yaml bundled into one JSON document, built once
// at start: the key order of the YAML is kept, and every reference into
// ../schemas/ is resolved inside the document. The envelope schema becomes
// components.schemas.Envelope (a fragment ref keeps its fragment under it);
// each kind schema is inlined as the component that referenced it
// (FrictionPayload, ReviewPayload), and refs to it elsewhere point there.
var openAPIDoc = func() []byte {
	doc, err := bundleOpenAPI(docs.OpenAPI)
	if err != nil {
		panic("api: bundle openapi.yaml: " + err.Error())
	}
	return doc
}()

const envelopeFile = "../schemas/envelope.v1.json"

// kindComponents maps each kind schema file to the component that inlines it.
var kindComponents = map[string]string{
	"../schemas/kinds/friction.v1.json": "FrictionPayload",
	"../schemas/kinds/review.v1.json":   "ReviewPayload",
}

func bundleOpenAPI(src []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return nil, err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return nil, fmt.Errorf("not a single YAML document")
	}
	envelope, err := schemaFile(strings.TrimPrefix(envelopeFile, "../schemas/"))
	if err != nil {
		return nil, err
	}
	b := &bundler{}
	if err := b.node(root.Content[0], nil, envelope); err != nil {
		return nil, err
	}
	if !b.envelopeAdded {
		return nil, fmt.Errorf("components.schemas not found")
	}
	var out bytes.Buffer
	if err := json.Compact(&out, b.buf.Bytes()); err != nil {
		return nil, fmt.Errorf("bundle is not valid JSON: %w", err)
	}
	return out.Bytes(), nil
}

func schemaFile(name string) ([]byte, error) {
	raw, err := schemas.FS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out.Bytes(), nil
}

type bundler struct {
	buf           bytes.Buffer
	envelopeAdded bool
}

// node writes n as JSON; path is the mapping keys leading to n.
func (b *bundler) node(n *yaml.Node, path []string, envelope []byte) error {
	switch n.Kind {
	case yaml.AliasNode:
		return b.node(n.Alias, path, envelope)
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return err
		}
		return b.value(v)
	case yaml.SequenceNode:
		b.buf.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				b.buf.WriteByte(',')
			}
			if err := b.node(c, append(path, "[]"), envelope); err != nil {
				return err
			}
		}
		b.buf.WriteByte(']')
		return nil
	case yaml.MappingNode:
		return b.mapping(n, path, envelope)
	}
	return fmt.Errorf("unsupported YAML node kind %d at %s", n.Kind, strings.Join(path, "."))
}

func (b *bundler) mapping(n *yaml.Node, path []string, envelope []byte) error {
	b.buf.WriteByte('{')
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		if i > 0 {
			b.buf.WriteByte(',')
		}
		if err := b.value(key); err != nil {
			return err
		}
		b.buf.WriteByte(':')
		if key == "$ref" && val.Kind == yaml.ScalarNode && !strings.HasPrefix(val.Value, "#") {
			ref, err := rewriteRef(val.Value)
			if err != nil {
				return err
			}
			if err := b.value(ref); err != nil {
				return err
			}
			continue
		}
		child := append(path[:len(path):len(path)], key)
		// A component that is only a ref to a kind file becomes that file.
		if len(child) == 3 && child[0] == "components" && child[1] == "schemas" {
			file, ok, err := kindRef(val)
			if err != nil {
				return fmt.Errorf("components.schemas.%s: %w", key, err)
			}
			if ok {
				if kindComponents[file] != key {
					return fmt.Errorf("component %s references %s, bundled as %s", key, file, kindComponents[file])
				}
				doc, err := schemaFile(strings.TrimPrefix(file, "../schemas/"))
				if err != nil {
					return err
				}
				b.buf.Write(doc)
				continue
			}
		}
		if err := b.node(val, child, envelope); err != nil {
			return err
		}
	}
	if len(path) == 2 && path[0] == "components" && path[1] == "schemas" {
		if len(n.Content) > 0 {
			b.buf.WriteByte(',')
		}
		b.buf.WriteString(`"Envelope":`)
		b.buf.Write(envelope)
		b.envelopeAdded = true
	}
	b.buf.WriteByte('}')
	return nil
}

// kindRef reports whether n is exactly {$ref: <kind schema file>}. A
// mapping that refs a kind file beside other keys is an error: bundling it
// would rewrite the ref into a reference to the component itself.
func kindRef(n *yaml.Node) (string, bool, error) {
	if n.Kind != yaml.MappingNode {
		return "", false, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "$ref" {
			continue
		}
		file := n.Content[i+1].Value
		if _, ok := kindComponents[file]; !ok {
			return "", false, nil
		}
		if len(n.Content) != 2 {
			return "", false, fmt.Errorf("component references %s beside other keys; a kind schema must be referenced alone", file)
		}
		return file, true, nil
	}
	return "", false, nil
}

// rewriteRef maps an external ref onto the bundle; any ref outside the
// shipped schema files is an error.
func rewriteRef(ref string) (string, error) {
	file, frag, _ := strings.Cut(ref, "#")
	if file == envelopeFile {
		return "#/components/schemas/Envelope" + frag, nil
	}
	if comp, ok := kindComponents[file]; ok {
		return "#/components/schemas/" + comp + frag, nil
	}
	return "", fmt.Errorf("external $ref %q cannot be bundled", ref)
}

func (b *bundler) value(v any) error {
	enc := json.NewEncoder(&b.buf)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
