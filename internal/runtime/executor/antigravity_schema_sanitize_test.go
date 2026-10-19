package executor

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	antigravitychat "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/openai/chat-completions"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const sanitizeTestPayload = `{
  "request": {
    "contents": [
      {"role": "model", "parts": [{"functionCall": {"name": "manage_todo_list", "args": {
        "operation": "write",
        "todoList": [
          {"id": 1, "title": "output 1", "description": "d1", "status": "not-started"},
          {"id": 2, "title": "output 2", "description": "d2", "status": "not-started"}
        ]}}}]},
      {"role": "model", "parts": [{"functionCall": {"name": "write_file", "args": {
        "path": "a.md", "format": "markdown", "default": "x", "pattern": "p",
        "const": "c", "deprecated": false, "nullable": "n", "examples": "e",
        "additionalProperties": "ap", "x-custom": "keepme"
      }}}]}
    ],
    "tools": [{"functionDeclarations": [{
      "name": "manage_todo_list",
      "parametersJsonSchema": {
        "type": "object",
        "required": ["todoList"],
        "properties": {"todoList": {"type": "array", "items": {
          "type": "object",
          "required": ["id", "title"],
          "title": "TodoItem",
          "properties": {"id": {"type": "number"}, "title": {"type": "string", "minLength": 3}}
        }}}
      }
    }]}]
  }
}`

func buildRequestBodyFromRawPayload(t *testing.T, modelName string, payload []byte) map[string]any {
	t.Helper()

	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "test-project"}}
	payload, err := executor.prepareAntigravityRequestPayload(context.Background(), auth,
		cliproxyexecutor.Request{Model: modelName}, cliproxyexecutor.Options{}, payload, payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := executor.buildRequest(
		context.Background(), auth, "token", payload, false, "", "https://example.com",
	)
	if err != nil {
		t.Fatalf("buildRequest error: %v", err)
	}
	raw, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read request body error: %v", errRead)
	}
	var body map[string]any
	if errUnmarshal := json.Unmarshal(raw, &body); errUnmarshal != nil {
		t.Fatalf("unmarshal request body error: %v, body=%s", errUnmarshal, raw)
	}
	return body
}

// TestSanitizeAntigravityRequestSchemasPreservesHistory guards against the schema cleaner being
// applied to the whole payload, which silently stripped keys such as "title" from functionCall
// arguments replayed from conversation history.
func TestSanitizeAntigravityRequestSchemasPreservesHistory(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		useAntigravitySchema bool
	}{
		{"gemini", false},
		{"antigravity", true},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				got := sanitizeAntigravityRequestSchemas(sanitizeTestPayload, tc.useAntigravitySchema)

				before := gjson.Get(sanitizeTestPayload, "request.contents")
				after := gjson.Get(got, "request.contents")
				if before.Raw != after.Raw {
					t.Errorf("conversation history was mutated.\nbefore: %s\nafter:  %s", before.Raw, after.Raw)
				}

				todo := gjson.Get(got, `request.contents.0.parts.0.functionCall.args.todoList`)
				for i, item := range todo.Array() {
					if !item.Get("title").Exists() {
						t.Errorf("todoList[%d] lost its title: %s", i, item.Raw)
					}
				}

				args := gjson.Get(got, `request.contents.1.parts.0.functionCall.args`)
				for _, key := range []string{
					"format", "default", "pattern", "const", "deprecated", "examples", "additionalProperties",
					"x-custom",
				} {
					if !args.Get(gjson.Escape(key)).Exists() {
						t.Errorf("argument key %q was stripped from history: %s", key, args.Raw)
					}
				}
				if args.Get("enum").Exists() {
					t.Errorf("cleaner fabricated an enum key in history args: %s", args.Raw)
				}
			},
		)
	}
}

// TestSanitizeAntigravityRequestSchemasStillCleansSchemas verifies the schema itself is still
// renamed and cleaned, so scoping the cleaner did not disable it.
func TestSanitizeAntigravityRequestSchemasStillCleansSchemas(t *testing.T) {
	got := sanitizeAntigravityRequestSchemas(sanitizeTestPayload, false)

	decl := "request.tools.0.functionDeclarations.0"
	if gjson.Get(got, decl+".parametersJsonSchema").Exists() {
		t.Errorf("parametersJsonSchema was not renamed: %s", gjson.Get(got, decl).Raw)
	}
	schema := gjson.Get(got, decl+".parameters")
	if !schema.Exists() {
		t.Fatalf("parameters missing after sanitization: %s", gjson.Get(got, decl).Raw)
	}

	items := schema.Get("properties.todoList.items")
	if items.Get("title").Exists() {
		t.Errorf("schema keyword title was not removed: %s", items.Raw)
	}
	if items.Get("properties.title.minLength").Exists() {
		t.Errorf("unsupported keyword minLength was not removed: %s", items.Raw)
	}
	if !items.Get("properties.title").Exists() {
		t.Errorf("schema property named title must be preserved: %s", items.Raw)
	}
	if req := items.Get("required").Array(); len(req) != 2 {
		t.Errorf("required list should keep id and title, got: %s", items.Get("required").Raw)
	}
}

// TestSanitizeAntigravityRequestSchemasCleansResultSchemas covers the schemas a function
// declaration can carry besides its parameters. Missing one sends it upstream uncleaned.
func TestSanitizeAntigravityRequestSchemasCleansResultSchemas(t *testing.T) {
	payload := `{"request": {"tools": [{"functionDeclarations": [{
	  "name": "t",
	  "parameters": {"type": "object", "$id": "drop-a", "properties": {"a": {"type": "string"}}},
	  "response": {"type": "object", "$comment": "drop-b", "properties": {"b": {"type": "string"}}},
	  "responseJsonSchema": {"type": "object", "$id": "drop-c", "properties": {"c": {"type": "string"}}}
	}]}]}}`

	got := sanitizeAntigravityRequestSchemas(payload, false)
	decl := gjson.Get(got, "request.tools.0.functionDeclarations.0")

	for _, unsupported := range []string{`parameters.\$id`, `response.\$comment`, `responseJsonSchema.\$id`} {
		if decl.Get(unsupported).Exists() {
			t.Errorf("unsupported keyword %s survived cleaning: %s", unsupported, decl.Raw)
		}
	}
	for _, kept := range []string{
		"parameters.properties.a", "response.properties.b", "responseJsonSchema.properties.c",
	} {
		if !decl.Get(kept).Exists() {
			t.Errorf("%s should be preserved: %s", kept, decl.Raw)
		}
	}
}

// TestAntigravitySchemaPathsCoverEverySchemaLocation pins every payload location cleaned by the sanitizer.
// A schema-bearing location missing from this list can send unsupported fields upstream.
// Keep these declaration keys aligned with allowedToolKeys in the Antigravity Claude request translator.
func TestAntigravitySchemaPathsCoverEverySchemaLocation(t *testing.T) {
	const schema = `{"type":"object","$id":"drop","properties":{"a":{"type":"string"}}}`

	// Both spellings of the declarations container are exercised: the Gemini translator forwards
	// snake_case untouched, so covering only camelCase leaves those requests uncleaned.
	for _, declContainer := range []string{"functionDeclarations", "function_declarations"} {
		for _, genContainer := range antigravityGenerationConfigContainers {
			t.Run(
				declContainer+"_"+strings.TrimPrefix(genContainer, "request."), func(t *testing.T) {
					var decl strings.Builder
					decl.WriteString(`"name":"t"`)
					for _, k := range antigravityDeclarationSchemaKeys {
						decl.WriteString(`,"` + k + `":` + schema)
					}
					var gen strings.Builder
					for i, k := range antigravityGenerationSchemaKeys {
						if i > 0 {
							gen.WriteString(",")
						}
						gen.WriteString(`"` + k + `":` + schema)
					}
					payload := `{"request":{"tools":[{"` + declContainer + `":[{` + decl.String() + `}]}],"` +
						strings.TrimPrefix(genContainer, "request.") + `":{` + gen.String() + `}}}`

					if !antigravityRequestNeedsSchemaSanitization([]byte(payload)) {
						t.Fatal("sanitization must trigger for a payload carrying schemas")
					}
					got := sanitizeAntigravityRequestSchemas(payload, false)

					check := func(path string) {
						t.Helper()
						node := gjson.Get(got, path)
						if !node.Exists() {
							t.Errorf("%s disappeared: %s", path, got)
							return
						}
						if node.Get(`\$id`).Exists() {
							t.Errorf("%s was never cleaned, $id reaches upstream: %s", path, node.Raw)
						}
					}
					base := "request.tools.0." + declContainer + ".0."
					for _, k := range antigravityDeclarationSchemaKeys {
						// Only the camelCase alias is renamed onto parameters, matching whole-payload
						// cleaning. Every other spelling is cleaned where the client put it.
						if k == "parametersJsonSchema" {
							if gjson.Get(got, base+k).Exists() {
								t.Errorf("%s should have been renamed onto parameters: %s", k, got)
							}
							continue
						}
						check(base + k)
					}
					for _, k := range antigravityGenerationSchemaKeys {
						check(genContainer + "." + k)
					}
				},
			)
		}
	}
}

// TestSanitizeAntigravityRequestSchemasMatchesWholePayloadCleaning pins the emitted schema to what
// whole-payload cleaning produced. Narrowing the scope must change which nodes are cleaned, never
// the result for a schema node — in particular the Claude VALIDATED placeholder, which the cleaner
// only adds when the schema is not top-level.
func TestSanitizeAntigravityRequestSchemasMatchesWholePayloadCleaning(t *testing.T) {
	shapes := map[string]string{
		"optionalOnly": `{"type":"object","properties":{"flag":{"type":"string"}}}`,
		"emptyProps":   `{"type":"object","properties":{}}`,
		"noProps":      `{"type":"object"}`,
		"withRequired": `{"type":"object","required":["a"],"properties":{"a":{"type":"string","minLength":2}}}`,
		"nestedArray":  `{"type":"object","properties":{"list":{"type":"array","items":{"type":"object","title":"X","required":["id","title"],"properties":{"id":{"type":"number"},"title":{"type":"string"}}}}}}`,
		"enumAndRemoved": `{"type":"object","$comment":"c","properties":{"m":{"type":"string","enum":["a","b"],` +
			`"deprecated":true}}}`,
	}
	const schemaPath = "request.tools.0.functionDeclarations.0.parameters"

	for _, useAntigravitySchema := range []bool{false, true} {
		for name, schema := range shapes {
			doc := `{"request":{"tools":[{"functionDeclarations":[{"name":"t","parameters":` + schema + `}]}]}}`
			whole := util.CleanJSONSchemaForGemini(doc)
			if useAntigravitySchema {
				whole = util.CleanJSONSchemaForAntigravity(doc)
			}
			want := gjson.Get(whole, schemaPath).Raw
			got := gjson.Get(sanitizeAntigravityRequestSchemas(doc, useAntigravitySchema), schemaPath).Raw
			if want != got {
				t.Errorf(
					"%s (antigravity=%v) diverged from whole-payload cleaning.\nwant: %s\ngot:  %s",
					name, useAntigravitySchema, want, got,
				)
			}
		}
	}

	// Explicitly pin the placeholder, so the equivalence above cannot pass by both sides dropping it.
	doc := `{"request":{"tools":[{"functionDeclarations":[{"name":"t","parameters":` + shapes["optionalOnly"] + `}]}]}}`
	got := gjson.Get(sanitizeAntigravityRequestSchemas(doc, true), schemaPath)
	if req := got.Get("required").Array(); len(req) != 1 || req[0].String() != "_" {
		t.Errorf("Claude VALIDATED placeholder missing for an optional-only schema: %s", got.Raw)
	}
}

func TestSanitizeAntigravityRequestSchemasKeepsResponseSchemasPlaceholderFree(t *testing.T) {
	payload := `{"request":{
		"tools":[{"functionDeclarations":[{"name":"tool","parameters":{"type":"object","properties":{"value":{"type":"string"}}}}]}],
		"generationConfig":{"responseSchema":{"type":"object","properties":{
			"empty":{"type":"object"},
			"optional":{"type":"object","properties":{"value":{"type":"string"}}}
		}}}
	}}`

	got := sanitizeAntigravityRequestSchemas(payload, true)
	toolSchema := gjson.Get(got, "request.tools.0.functionDeclarations.0.parameters")
	if required := toolSchema.Get("required.0").String(); required != "_" {
		t.Fatalf("tool schema lost VALIDATED placeholder, required[0] = %q: %s", required, got)
	}

	responseSchema := gjson.Get(got, "request.generationConfig.responseSchema")
	for _, path := range []string{
		"required",
		"properties._",
		"properties.reason",
		"properties.empty.required",
		"properties.empty.properties.reason",
		"properties.optional.required",
		"properties.optional.properties._",
	} {
		if responseSchema.Get(path).Exists() {
			t.Errorf("response schema gained tool-only field %s: %s", path, responseSchema.Raw)
		}
	}
}

func TestAntigravityBuildRequestKeepsJSONObjectMimeOnly(t *testing.T) {
	input := []byte(`{"model":"gemini-3.1-pro-low","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	translated := antigravitychat.ConvertOpenAIRequestToAntigravity("gemini-3.1-pro-low", input, false)
	body := buildRequestBodyFromRawPayload(t, "gemini-3.1-pro-low", translated)
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	generationConfig := gjson.GetBytes(encoded, "request.generationConfig")
	if got := generationConfig.Get("responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json: %s", got, encoded)
	}
	if generationConfig.Get("responseSchema").Exists() {
		t.Fatalf("responseSchema should not be set for json_object: %s", encoded)
	}
}

func TestAntigravityBuildRequestPreservesGenerationResponseSchemaMetadata(t *testing.T) {
	payload := []byte(`{"request":{"generationConfig":{"responseSchema":{
		"type":"object",
		"nullable":true,
		"properties":{"_":{"type":"string","nullable":true}},
		"required":["_"]
	}}}}`)

	for _, modelName := range []string{"gemini-3.6-flash-high", "gemini-3.1-pro-low"} {
		t.Run(
			modelName, func(t *testing.T) {
				body := buildRequestBodyFromRawPayload(t, modelName, payload)
				encoded, errMarshal := json.Marshal(body)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}

				schema := gjson.GetBytes(encoded, "request.generationConfig.responseSchema")
				if !schema.Get("nullable").Bool() || !schema.Get("properties._.nullable").Bool() {
					t.Fatalf("response schema nullable metadata was removed: %s", schema.Raw)
				}
				if !schema.Get("properties._").Exists() {
					t.Fatalf("legitimate underscore property was removed: %s", schema.Raw)
				}
				if required := schema.Get("required.0").String(); required != "_" {
					t.Fatalf("required[0] = %q, want underscore: %s", required, schema.Raw)
				}
			},
		)
	}
}

func TestAntigravityBuildRequestSanitizesSnakeCaseGenerationResponseSchemas(t *testing.T) {
	for _, testCase := range []struct {
		alias     string
		canonical string
	}{
		{alias: "response_schema", canonical: "responseSchema"},
		{alias: "response_json_schema", canonical: "responseJsonSchema"},
	} {
		t.Run(
			testCase.alias, func(t *testing.T) {
				payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"` + testCase.alias + `":{"type":"object","$id":"drop-me","properties":{"title":{"type":"string"}}}}}}`)
				body := buildRequestBodyFromRawPayload(t, "gemini-3.6-flash-high", payload)
				encoded, errMarshal := json.Marshal(body)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}

				base := "request.generation_config."
				// The upstream API accepts either spelling, so the field must stay where the client put
				// it. Only the unsupported keywords inside it are what upstream rejects.
				schema := gjson.GetBytes(encoded, base+testCase.alias)
				if !schema.Exists() {
					t.Fatalf("snake_case response schema was renamed or dropped: %s", encoded)
				}
				if gjson.GetBytes(encoded, base+testCase.canonical).Exists() {
					t.Fatalf("cleaning must not add a second spelling: %s", encoded)
				}
				if schema.Get(`\$id`).Exists() {
					t.Fatalf("unsupported $id survived cleaning: %s", schema.Raw)
				}
				if !schema.Get("properties.title").Exists() {
					t.Fatalf("schema property named title was removed: %s", schema.Raw)
				}
			},
		)
	}
}

// TestSanitizeAntigravityRequestSchemasCleansBothSpellingsInPlace covers a payload carrying both
// spellings: each is cleaned where it sits, and neither is silently dropped.
func TestSanitizeAntigravityRequestSchemasCleansBothSpellingsInPlace(t *testing.T) {
	payload := `{"request":{"generationConfig":{` +
		`"responseSchema":{"type":"object","$id":"drop-a","properties":{"canonical":{"type":"string"}}},` +
		`"response_schema":{"type":"object","$id":"drop-b","properties":{"alias":{"type":"string"}}}}}}`

	got := sanitizeAntigravityRequestSchemas(payload, false)

	for path, prop := range map[string]string{
		"request.generationConfig.responseSchema":  "canonical",
		"request.generationConfig.response_schema": "alias",
	} {
		schema := gjson.Get(got, path)
		if !schema.Exists() {
			t.Errorf("%s was dropped: %s", path, got)
			continue
		}
		if schema.Get(`\$id`).Exists() {
			t.Errorf("%s kept unsupported $id: %s", path, schema.Raw)
		}
		if !schema.Get("properties." + prop).Exists() {
			t.Errorf("%s lost its property: %s", path, schema.Raw)
		}
	}
}

// TestSanitizeAntigravityRequestSchemasIsIdempotent guards the hint duplication seen in
// production, where a schema cleaned by a translator was cleaned again by this executor.
func TestSanitizeAntigravityRequestSchemasIsIdempotent(t *testing.T) {
	// "withDesc" already has a description, so the hint is parenthesized; "bare" has none, so the
	// hint is stored on its own. Both spellings must survive a second cleaning pass unchanged.
	// "compound" has no description and two hints, so the first pass stores the enum hint bare and
	// appends the constraint after it — the second pass must recognize that leading bare form.
	payload := `{"request": {"tools": [{"functionDeclarations": [{
	  "name": "manage_todo_list",
	  "parameters": {"type": "object", "properties": {
	    "withDesc": {"type": "string", "enum": ["write", "read"], "description": "pick one"},
	    "bare": {"type": "string", "enum": ["not-started", "in-progress", "completed"]},
	    "compound": {"type": "string", "enum": ["a", "b"], "minLength": 1}}}
	}]}]}}`

	once := sanitizeAntigravityRequestSchemas(payload, false)
	twice := sanitizeAntigravityRequestSchemas(once, false)

	base := "request.tools.0.functionDeclarations.0.parameters.properties."
	for _, prop := range []string{"withDesc", "bare", "compound"} {
		descPath := base + prop + ".description"
		first, second := gjson.Get(once, descPath).String(), gjson.Get(twice, descPath).String()
		if first != second {
			t.Errorf("%s: cleaning is not idempotent.\nonce:  %s\ntwice: %s", prop, first, second)
		}
		if strings.Count(second, "Allowed:") != 1 {
			t.Errorf("%s: hint duplicated: %s", prop, second)
		}
	}
}

var propertyNamesShapes = map[string]string{
	"arrayItem": `{"type":"object","properties":{"records":{"type":"array","items":{"type":"object",` +
		`"properties":{"name":{"type":"string"}},"propertyNames":{"type":"string"}}}}}`,
	"propertyNamedProperties": `{"type":"object","properties":{"properties":{"type":"object",` +
		`"propertyNames":{"type":"string"}}}}`,
}

func TestSanitizeAntigravityRequestSchemasStripsPropertyNamesEverywhere(t *testing.T) {
	for shapeName, schema := range propertyNamesShapes {
		for _, declarationContainer := range []string{"functionDeclarations", "function_declarations"} {
			for _, generationContainer := range antigravityGenerationConfigContainers {
				name := shapeName + "_" + declarationContainer + "_" + strings.TrimPrefix(
					generationContainer, "request.",
				)
				t.Run(
					name, func(t *testing.T) {
						var declaration strings.Builder
						declaration.WriteString(`"name":"t"`)
						for _, key := range antigravityDeclarationSchemaKeys {
							declaration.WriteString(`,"` + key + `":` + schema)
						}
						var generation strings.Builder
						for i, key := range antigravityGenerationSchemaKeys {
							if i > 0 {
								generation.WriteString(",")
							}
							generation.WriteString(`"` + key + `":` + schema)
						}
						payload := `{"request":{"tools":[{"` + declarationContainer + `":[{` + declaration.String() + `}]}],"` +
							strings.TrimPrefix(generationContainer, "request.") + `":{` + generation.String() + `}}}`

						for _, useAntigravitySchema := range []bool{false, true} {
							got := sanitizeAntigravityRequestSchemas(payload, useAntigravitySchema)
							if strings.Contains(got, `"propertyNames"`) {
								t.Errorf(
									"antigravity=%v: propertyNames reaches upstream: %s", useAntigravitySchema, got,
								)
							}
						}
					},
				)
			}
		}
	}
}

func TestSanitizeAntigravityRequestSchemasKeepsPropertyNamesInHistory(t *testing.T) {
	payload := `{"request":{
		"contents":[{"role":"model","parts":[{"functionCall":{"name":"t","args":{
			"propertyNames":"keep-me",
			"properties":{"propertyNames":"keep-me-too"}
		}}}]}],
		"tools":[{"functionDeclarations":[{"name":"t","parameters":{"type":"object","properties":{
			"propertyNames":{"type":"string"},
			"properties":{"type":"object","propertyNames":{"type":"string"}}
		}}}]}]
	}}`

	for _, useAntigravitySchema := range []bool{false, true} {
		got := sanitizeAntigravityRequestSchemas(payload, useAntigravitySchema)
		before := gjson.Get(payload, "request.contents")
		after := gjson.Get(got, "request.contents")
		if before.Raw != after.Raw {
			t.Errorf(
				"antigravity=%v: history was mutated.\nbefore: %s\nafter:  %s", useAntigravitySchema, before.Raw,
				after.Raw,
			)
		}

		schema := gjson.Get(got, "request.tools.0.functionDeclarations.0.parameters")
		if !schema.Get("properties.propertyNames").Exists() {
			t.Errorf("antigravity=%v: property named propertyNames was removed: %s", useAntigravitySchema, schema.Raw)
		}
		if schema.Get("properties.properties.propertyNames").Exists() {
			t.Errorf(
				"antigravity=%v: propertyNames keyword survived inside a property named properties: %s",
				useAntigravitySchema, schema.Raw,
			)
		}
	}
}

func TestAntigravityBuildRequestStripsPropertyNamesFromOutboundBody(t *testing.T) {
	for shapeName, schema := range propertyNamesShapes {
		for _, modelName := range []string{"gemini-3.1-pro", "claude-opus-4-6"} {
			t.Run(
				shapeName+"_"+modelName, func(t *testing.T) {
					payload := []byte(`{"request":{
					"contents":[{"role":"model","parts":[{"functionCall":{"name":"t","args":{"propertyNames":"keep-me"}}}]}],
					"tools":[{"function_declarations":[{"name":"t","parametersJsonSchema":` + schema + `}]}],
					"generationConfig":{"responseSchema":` + schema + `}
				}}`)

					body := buildRequestBodyFromRawPayload(t, modelName, payload)
					encoded, errMarshal := json.Marshal(body)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					for _, path := range []string{"request.tools", "request.generationConfig"} {
						if node := gjson.GetBytes(encoded, path); strings.Contains(node.Raw, `"propertyNames"`) {
							t.Errorf("%s still carries propertyNames: %s", path, node.Raw)
						}
					}
					args := gjson.GetBytes(encoded, "request.contents.0.parts.0.functionCall.args")
					if args.Get("propertyNames").String() != "keep-me" {
						t.Errorf("functionCall argument named propertyNames was rewritten: %s", args.Raw)
					}
				},
			)
		}
	}
}

func TestSanitizeAntigravityRequestSchemasNormalizesTrueBooleanSubschemas(t *testing.T) {
	payload := `{"request":{"tools":[{"functionDeclarations":[{"name":"t","parametersJsonSchema":{
		"type":"object","properties":{
			"flag":true,"disabled":false,
			"nested":{"type":"object","properties":{"flag":true}},
			"tags":{"type":"array","items":true},
			"variants":{"anyOf":[true,{"type":"string"}]},
			"large":9007199254740993
		},"additionalProperties":true
	}}]}]}}`
	for _, useAntigravitySchema := range []bool{false, true} {
		output := sanitizeAntigravityRequestSchemas(payload, useAntigravitySchema)
		params := gjson.Get(output, "request.tools.0.functionDeclarations.0.parameters")
		for _, path := range []string{"properties.flag", "properties.nested.properties.flag", "properties.tags.items"} {
			if got := params.Get(path); !got.IsObject() || got.Raw != "{}" {
				t.Errorf("antigravity=%t: %s = %s, want {}; output=%s", useAntigravitySchema, path, got.Raw, output)
			}
		}
		if got := params.Get("properties.disabled"); got.Type != gjson.False {
			t.Errorf("antigravity=%t: false schema changed: %s", useAntigravitySchema, got.Raw)
		}
		if got := params.Get("properties.large"); got.Raw != "9007199254740993" {
			t.Errorf("antigravity=%t: large integer changed: %s", useAntigravitySchema, got.Raw)
		}
	}
}
