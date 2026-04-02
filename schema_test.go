package main

import (
	"encoding/json"
	"testing"
)

func TestParseInputSchema_Basic(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"},
			"limit": {"type": "number", "description": "Max results"}
		},
		"required": ["query"]
	}`)

	params, skipped := parseInputSchema(schema)
	if len(params) != 2 {
		t.Fatalf("expected 2 params, got %d", len(params))
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped, got %d", skipped)
	}

	// Sorted by name: limit, query
	if params[0].Name != "limit" {
		t.Errorf("expected first param 'limit', got %q", params[0].Name)
	}
	if params[0].Type != "number" {
		t.Errorf("expected type 'number', got %q", params[0].Type)
	}
	if params[0].Required {
		t.Error("expected limit not required")
	}

	if params[1].Name != "query" {
		t.Errorf("expected second param 'query', got %q", params[1].Name)
	}
	if !params[1].Required {
		t.Error("expected query required")
	}
	if params[1].Description != "Search query" {
		t.Errorf("expected description 'Search query', got %q", params[1].Description)
	}
}

func TestParseInputSchema_SkipsComplexTypes(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"metadata": {"type": "object"},
			"tags": {"type": "array"}
		}
	}`)

	params, skipped := parseInputSchema(schema)
	if len(params) != 1 {
		t.Fatalf("expected 1 param (complex types skipped), got %d", len(params))
	}
	if skipped != 2 {
		t.Errorf("expected 2 skipped, got %d", skipped)
	}
	if params[0].Name != "name" {
		t.Errorf("expected 'name', got %q", params[0].Name)
	}
}

func TestParseInputSchema_WithDefaults(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"limit": {"type": "integer", "default": 10}
		}
	}`)

	params, _ := parseInputSchema(schema)
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if params[0].Default == nil {
		t.Error("expected default value")
	}
	// JSON numbers unmarshal as float64
	if params[0].Default.(float64) != 10 {
		t.Errorf("expected default 10, got %v", params[0].Default)
	}
}

func TestParseInputSchema_WithEnum(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"format": {"type": "string", "enum": ["json", "csv", "xml"]}
		}
	}`)

	params, _ := parseInputSchema(schema)
	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}
	if len(params[0].Enum) != 3 {
		t.Errorf("expected 3 enum values, got %d", len(params[0].Enum))
	}
}

func TestParseInputSchema_Empty(t *testing.T) {
	params, skipped := parseInputSchema(nil)
	if len(params) != 0 {
		t.Errorf("expected 0 params for nil schema, got %d", len(params))
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped for nil schema, got %d", skipped)
	}

	params, skipped = parseInputSchema(json.RawMessage(`{}`))
	if len(params) != 0 {
		t.Errorf("expected 0 params for empty schema, got %d", len(params))
	}
	if skipped != 0 {
		t.Errorf("expected 0 skipped for empty schema, got %d", skipped)
	}
}

func TestParseInputSchema_AllTypes(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"count": {"type": "integer"},
			"score": {"type": "number"},
			"verbose": {"type": "boolean"}
		}
	}`)

	params, _ := parseInputSchema(schema)
	if len(params) != 4 {
		t.Fatalf("expected 4 params, got %d", len(params))
	}

	types := map[string]string{}
	for _, p := range params {
		types[p.Name] = p.Type
	}
	if types["name"] != "string" || types["count"] != "integer" ||
		types["score"] != "number" || types["verbose"] != "boolean" {
		t.Errorf("unexpected types: %v", types)
	}
}

func TestCoerceDynamicFlags_Types(t *testing.T) {
	params := []toolParam{
		{Name: "query", Type: "string"},
		{Name: "limit", Type: "number"},
		{Name: "count", Type: "integer"},
		{Name: "verbose", Type: "boolean"},
	}

	flags := map[string]string{
		"query":   "hello",
		"limit":   "3.14",
		"count":   "42",
		"verbose": "true",
	}

	result, err := coerceDynamicFlags(flags, params)
	if err != nil {
		t.Fatal(err)
	}

	if result["query"] != "hello" {
		t.Errorf("query: expected 'hello', got %v", result["query"])
	}
	if result["limit"] != 3.14 {
		t.Errorf("limit: expected 3.14, got %v", result["limit"])
	}
	if result["count"] != 42 {
		t.Errorf("count: expected 42, got %v", result["count"])
	}
	if result["verbose"] != true {
		t.Errorf("verbose: expected true, got %v", result["verbose"])
	}
}

func TestCoerceDynamicFlags_InvalidNumber(t *testing.T) {
	params := []toolParam{{Name: "limit", Type: "number"}}
	flags := map[string]string{"limit": "abc"}

	_, err := coerceDynamicFlags(flags, params)
	if err == nil {
		t.Fatal("expected error for invalid number")
	}
}

func TestCoerceDynamicFlags_UnknownFlag(t *testing.T) {
	params := []toolParam{{Name: "query", Type: "string"}}
	flags := map[string]string{"unknown": "value"}

	result, err := coerceDynamicFlags(flags, params)
	if err != nil {
		t.Fatal(err)
	}
	if result["unknown"] != "value" {
		t.Errorf("expected unknown flag passed as string, got %v", result["unknown"])
	}
}
