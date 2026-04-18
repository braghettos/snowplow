package api

import (
	"fmt"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// topologicalLevels groups APIs by dependency level using BFS. APIs within
// the same level have no dependencies on each other and can run in parallel.
// Level 0 = no dependencies, level 1 = depends only on level 0, etc.
func topologicalLevels(items []*templates.API) ([][]string, error) {
	graph := make(map[string][]string)
	inDegree := make(map[string]int)
	itemSet := make(map[string]bool)

	for _, item := range items {
		itemSet[item.Name] = true
		if item.DependsOn == nil {
			continue
		}
		if dep := item.DependsOn.Name; len(dep) > 0 {
			graph[dep] = append(graph[dep], item.Name)
			inDegree[item.Name]++
		}
	}

	var queue []string
	for item := range itemSet {
		if inDegree[item] == 0 {
			queue = append(queue, item)
		}
	}

	var levels [][]string
	visited := 0
	for len(queue) > 0 {
		levels = append(levels, queue)
		visited += len(queue)
		var next []string
		for _, item := range queue {
			for _, dep := range graph[item] {
				inDegree[dep]--
				if inDegree[dep] == 0 {
					next = append(next, dep)
				}
			}
		}
		queue = next
	}

	if visited != len(itemSet) {
		return nil, fmt.Errorf("cyclic dependency detected")
	}
	return levels, nil
}

