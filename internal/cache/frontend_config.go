package cache

import (
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type FrontendConfig struct {
	API struct {
		INIT         string `json:"INIT"`
		RoutesLoader string `json:"ROUTES_LOADER"`
	} `json:"api"`
}

type EntryPoint struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

func LoadFrontendConfig(path string) (*FrontendConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading frontend config %s: %w", path, err)
	}
	var cfg FrontendConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing frontend config: %w", err)
	}
	return &cfg, nil
}

func (fc *FrontendConfig) EntryPoints() []EntryPoint {
	var eps []EntryPoint
	for _, path := range []string{fc.API.INIT, fc.API.RoutesLoader} {
		if path == "" {
			continue
		}
		gvr, ns, name := ParseCallPath(path)
		if gvr.Resource == "" || name == "" {
			continue
		}
		eps = append(eps, EntryPoint{GVR: gvr, Namespace: ns, Name: name})
	}
	return eps
}
