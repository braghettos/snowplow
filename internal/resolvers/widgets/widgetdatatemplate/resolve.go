package widgetdatatemplate

import (
	"context"
	"runtime"
	"strings"
	"sync"

	"github.com/krateoplatformops/plumbing/jqutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	jqsupport "github.com/krateoplatformops/snowplow/internal/support/jq"
)

type EvalResult struct {
	Path  string
	Value any
}

type ResolveOptions struct {
	Items      []templatesv1.WidgetDataTemplate
	DataSource map[string]any
}

const parallelThreshold = 3

// estimateMapSize gives a rough byte estimate of a map[string]any tree
// to decide whether deepCopy is affordable. Not exact — just needs to
// distinguish 1KB from 100MB.
func estimateMapSize(v any) int {
	switch val := v.(type) {
	case map[string]any:
		size := 64 // map header
		for k, v := range val {
			size += len(k) + 16 + estimateMapSize(v)
			if size > 20*1024*1024 { // early exit
				return size
			}
		}
		return size
	case []any:
		size := 24 + len(val)*8
		for _, item := range val {
			size += estimateMapSize(item)
			if size > 20*1024*1024 {
				return size
			}
		}
		return size
	case string:
		return len(val) + 16
	default:
		return 16
	}
}

// deepCopyValue recursively clones map[string]any and []any trees so that
// each goroutine gets its own mutable copy. gojq.normalizeNumbers mutates
// input maps in-place (unconditionally writes v[k] = normalizeNumbers(x)
// for every key, even when values are already normalized), so sharing a
// single DataSource across goroutines causes concurrent map write panics
// or silent map corruption.
func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(val))
		for k, v := range val {
			m[k] = deepCopyValue(v)
		}
		return m
	case []any:
		s := make([]any, len(val))
		for i, v := range val {
			s[i] = deepCopyValue(v)
		}
		return s
	default:
		return v
	}
}

func Resolve(ctx context.Context, opts ResolveOptions) ([]EvalResult, error) {
	if len(opts.Items) == 0 {
		return []EvalResult{}, nil
	}

	type workItem struct {
		idx        int
		path       string
		expression string
	}

	work := make([]workItem, 0, len(opts.Items))
	for _, el := range opts.Items {
		if el.Expression == "" || el.ForPath == "" {
			continue
		}
		path := strings.TrimSpace(el.ForPath)
		if len(path) > 0 && path[0] == '.' {
			path = path[1:]
		}
		work = append(work, workItem{idx: len(work), path: path, expression: el.Expression})
	}

	if len(work) == 0 {
		return []EvalResult{}, nil
	}

	results := make([]EvalResult, len(work))

	eval := func(w workItem, ds map[string]any) error {
		s := w.expression
		if exp, ok := jqutil.MaybeQuery(w.expression); ok {
			val, err := jqutil.Eval(ctx, jqutil.EvalOptions{
				Query: exp, Data: ds, Unquote: false,
				ModuleLoader: jqsupport.ModuleLoader(),
			})
			if err != nil {
				return err
			}
			s = val
		}
		results[w.idx] = EvalResult{Path: w.path, Value: jqutil.InferType(s)}
		return nil
	}

	// At large scale (50K+ items), the DataSource can be 500MB+.
	// deepCopyValue per goroutine would allocate N×500MB.
	// Run sequentially when DataSource is large to avoid OOM.
	// The parallel path is only beneficial for small DataSources where
	// deepCopy cost is negligible.
	//
	// IMPORTANT: gojq.normalizeNumbers mutates input maps in-place
	// (unconditionally writes v[k] = normalizeNumbers(x) for every key,
	// even when values are already normalized). Sharing a single DataSource
	// across goroutines causes concurrent map write panics or silent
	// corruption (9.7GB heap / 35K goroutines with only 20 compositions).
	// Each goroutine MUST get its own deepCopy.
	estimatedSize := estimateMapSize(opts.DataSource)
	useParallel := len(work) >= parallelThreshold && estimatedSize < 10*1024*1024 // 10MB threshold

	if !useParallel {
		for _, w := range work {
			if err := eval(w, opts.DataSource); err != nil {
				return nil, err
			}
		}
		return results, nil
	}

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
		sem      = make(chan struct{}, runtime.GOMAXPROCS(0))
	)
	for _, w := range work {
		wg.Add(1)
		dsCopy := deepCopyValue(opts.DataSource).(map[string]any)
		sem <- struct{}{}
		go func(w workItem) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := eval(w, dsCopy); err != nil {
				errOnce.Do(func() { firstErr = err })
			}
		}(w)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}
