package api

import (
	"encoding/json"
	"strings"
	"testing"
)

const bundlerHead = `openapi: 3.1.0
info: {title: t, version: '1'}
paths:
  /x:
    get:
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema: {$ref: '%s'}
components:
  schemas:
`

func bundlerDoc(pathRef, schemas string) []byte {
	return []byte(strings.Replace(bundlerHead, "%s", pathRef, 1) + schemas)
}

func TestBundle_RewritesRefs(t *testing.T) {
	out, err := bundleOpenAPI(bundlerDoc("../schemas/kinds/friction.v1.json#/properties/category",
		"    FrictionPayload:\n      $ref: '../schemas/kinds/friction.v1.json'\n    Other:\n      $ref: '../schemas/envelope.v1.json#/properties/kind'\n"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema map[string]any
					}
				}
			}
		}
		Components struct {
			Schemas map[string]map[string]any
		}
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := doc.Paths["/x"].Get.Responses["200"].Content["application/json"].Schema["$ref"]
	if got != "#/components/schemas/FrictionPayload/properties/category" {
		t.Errorf("path ref = %v", got)
	}
	fp := doc.Components.Schemas["FrictionPayload"]
	if _, ok := fp["$ref"]; ok || len(fp) == 0 {
		t.Errorf("FrictionPayload not inlined: %v", fp)
	}
	if got := doc.Components.Schemas["Other"]["$ref"]; got != "#/components/schemas/Envelope/properties/kind" {
		t.Errorf("Other ref = %v", got)
	}
	if len(doc.Components.Schemas["Envelope"]) == 0 {
		t.Errorf("Envelope not added")
	}
}

func TestBundle_KindRefWithSiblingsIsAnError(t *testing.T) {
	_, err := bundleOpenAPI(bundlerDoc("#/components/schemas/FrictionPayload",
		"    FrictionPayload:\n      description: d\n      $ref: '../schemas/kinds/friction.v1.json'\n"))
	if err == nil || !strings.Contains(err.Error(), "FrictionPayload") {
		t.Fatalf("err = %v, want an error naming FrictionPayload", err)
	}
}

func TestBundle_UnknownExternalRefIsAnError(t *testing.T) {
	_, err := bundleOpenAPI(bundlerDoc("../schemas/kinds/nope.v1.json", "    A: {type: string}\n"))
	if err == nil || !strings.Contains(err.Error(), "nope.v1.json") {
		t.Fatalf("err = %v, want an unknown external ref error", err)
	}
}
