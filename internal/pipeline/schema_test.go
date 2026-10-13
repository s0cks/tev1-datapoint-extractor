package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A deliberately small JSON Schema checker covering the keywords used by
// schema/top.schema.json: type, const, enum, properties, required,
// additionalProperties, items, local $ref, minimum, maximum, pattern.
type schemaChecker struct{ root map[string]any }

func (c schemaChecker) resolve(s map[string]any) map[string]any {
	ref, ok := s["$ref"].(string)
	if !ok {
		return s
	}
	cur := any(c.root)
	for _, p := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		cur = cur.(map[string]any)[p]
	}
	return cur.(map[string]any)
}

func typesOf(v any) []string {
	switch x := v.(type) {
	case nil:
		return []string{"null"}
	case bool:
		return []string{"boolean"}
	case string:
		return []string{"string"}
	case []any:
		return []string{"array"}
	case map[string]any:
		return []string{"object"}
	case float64:
		if x == float64(int64(x)) {
			return []string{"integer", "number"}
		}
		return []string{"number"}
	}
	return nil
}

func (c schemaChecker) check(path string, s map[string]any, v any) []string {
	s = c.resolve(s)
	var errs []string
	if want, ok := s["type"]; ok {
		var allowed []string
		switch w := want.(type) {
		case string:
			allowed = []string{w}
		case []any:
			for _, x := range w {
				allowed = append(allowed, x.(string))
			}
		}
		okType := false
		for _, have := range typesOf(v) {
			for _, a := range allowed {
				okType = okType || a == have
			}
		}
		if !okType {
			return []string{fmt.Sprintf("%s: type %v, want %v", path, typesOf(v), allowed)}
		}
	}
	if k, ok := s["const"]; ok && k != v {
		errs = append(errs, fmt.Sprintf("%s: %v != const %v", path, v, k))
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || x == v
		}
		if !found {
			errs = append(errs, fmt.Sprintf("%s: %v not in enum", path, v))
		}
	}
	if p, ok := s["pattern"].(string); ok {
		if str, isStr := v.(string); isStr && !regexp.MustCompile(p).MatchString(str) {
			errs = append(errs, fmt.Sprintf("%s: %q !~ %s", path, str, p))
		}
	}
	if f, isNum := v.(float64); isNum {
		if m, ok := s["minimum"].(float64); ok && f < m {
			errs = append(errs, fmt.Sprintf("%s: %v < minimum %v", path, f, m))
		}
		if m, ok := s["maximum"].(float64); ok && f > m {
			errs = append(errs, fmt.Sprintf("%s: %v > maximum %v", path, f, m))
		}
	}
	if obj, isObj := v.(map[string]any); isObj {
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := obj[r.(string)]; !present {
					errs = append(errs, fmt.Sprintf("%s: missing required %q", path, r))
				}
			}
		}
		for k, val := range obj {
			if ps, ok := props[k].(map[string]any); ok {
				errs = append(errs, c.check(path+"."+k, ps, val)...)
			} else if ap, ok := s["additionalProperties"].(map[string]any); ok {
				errs = append(errs, c.check(path+"."+k, ap, val)...)
			} else if s["additionalProperties"] == false {
				errs = append(errs, fmt.Sprintf("%s: unexpected property %q", path, k))
			}
		}
	}
	if arr, isArr := v.([]any); isArr {
		if is, ok := s["items"].(map[string]any); ok {
			for i, el := range arr {
				errs = append(errs, c.check(fmt.Sprintf("%s[%d]", path, i), is, el)...)
			}
		}
	}
	return errs
}

func TestTopJSONMatchesSchema(t *testing.T) {
	raw, err := os.ReadFile("../../schema/top.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	sc := schemaChecker{root: root}

	f := newFake(t)
	for _, mode := range []string{"model", "no-model"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			o := opts(t, f, dir, writeInput(t, dir, sampleDoc))
			o.NoModel = mode == "no-model"
			if _, err := Run(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(dir, "out", "top.json"))
			var top any
			if err := json.Unmarshal(b, &top); err != nil {
				t.Fatal(err)
			}
			if errs := sc.check("$", root, top); len(errs) > 0 {
				t.Fatalf("top.json violates schema:\n%s", strings.Join(errs, "\n"))
			}
		})
	}
	// The checker itself must be able to fail.
	if errs := sc.check("$", root, map[string]any{"schema_version": "x"}); len(errs) == 0 {
		t.Fatal("checker accepted an invalid document")
	}
}
