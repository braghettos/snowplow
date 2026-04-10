package schema

import (
	"errors"
	"fmt"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"sigs.k8s.io/yaml"
)

func buildValidationFromSchemaData(data map[string]any) (*apiextensions.CustomResourceValidation, error) {
	// 1. Set additionalProperties=false
	enforceStrictObjects(data)

	// 2. From map to YAML
	yml, err := yaml.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal to YAML: %w", err)
	}

	var schemaV1 apiextv1.JSONSchemaProps
	if err := yaml.Unmarshal(yml, &schemaV1); err != nil {
		return nil, fmt.Errorf("unmarshal to v1 JSONSchemaProps: %w", err)
	}

	// 3. From v1.JSONSchemaProps to internal JSONSchemaProps
	var schemaInternal apiextensions.JSONSchemaProps
	if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		&schemaV1, &schemaInternal, nil); err != nil {
		return nil, fmt.Errorf("convert v1→internal: %w", err)
	}

	return &apiextensions.CustomResourceValidation{
		OpenAPIV3Schema: &schemaInternal,
	}, nil
}

// enforceStrictObjects set additionalProperties=false on all nodes (type: object)
func enforceStrictObjects(node map[string]any) {
	if nodeType, ok := node["type"].(string); ok && nodeType == "object" {
		// If preserve-unknown-fields exists, do not force additionalProperties
		preserve, ok := node["x-kubernetes-preserve-unknown-fields"].(bool)
		if !(ok && preserve) {
			if _, exists := node["additionalProperties"]; !exists {
				node["additionalProperties"] = false
			}
		}
	}

	// Recursion for properties
	if props, ok := node["properties"].(map[string]any); ok {
		for _, v := range props {
			if child, ok := v.(map[string]any); ok {
				enforceStrictObjects(child)
			}
		}
	}

	// Recursion for items (array)
	if items, ok := node["items"].(map[string]any); ok {
		enforceStrictObjects(items)
	} else if itemsSlice, ok := node["items"].([]any); ok {
		for _, it := range itemsSlice {
			if child, ok := it.(map[string]any); ok {
				enforceStrictObjects(child)
			}
		}
	}
}

/*
// enforceStrictObjects set additionalProperties=false on all nodes (type: object)
func enforceStrictObjects(node map[string]any) {
	if nodeType, ok := node["type"].(string); ok && nodeType == "object" {
		if _, exists := node["additionalProperties"]; !exists {
			node["additionalProperties"] = false
		}
	}

	// Recursion for properties
	if props, ok := node["properties"].(map[string]any); ok {
		for _, v := range props {
			if child, ok := v.(map[string]any); ok {
				enforceStrictObjects(child)
			}
		}
	}

	// Recursion for items (array)
	if items, ok := node["items"].(map[string]any); ok {
		enforceStrictObjects(items)
	} else if itemsSlice, ok := node["items"].([]any); ok {
		for _, it := range itemsSlice {
			if child, ok := it.(map[string]any); ok {
				enforceStrictObjects(child)
			}
		}
	}
}
*/

func validateCustomResource(crv *apiextensions.CustomResourceValidation, doc map[string]any) error {
	validator, _, err := validation.NewSchemaValidator(crv.OpenAPIV3Schema)
	if err != nil {
		return err
	}

	errs := validation.ValidateCustomResource(nil, doc, validator)
	if len(errs) == 0 {
		return nil
	}

	return errors.New(errs.ToAggregate().Error())
}

// validateCustomResourceWithCachedValidator uses a pre-compiled validator
// instead of re-building it on every call. Eliminates the NewSchemaValidator
// hot path that accounted for 47% of allocations in v0.25.161.
func validateCustomResourceWithCachedValidator(validator validation.SchemaValidator, doc map[string]any) error {
	errs := validation.ValidateCustomResource(nil, doc, validator)
	if len(errs) == 0 {
		return nil
	}
	return errors.New(errs.ToAggregate().Error())
}
