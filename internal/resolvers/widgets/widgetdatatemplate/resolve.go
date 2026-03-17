package widgetdatatemplate

import (
	"context"
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

// deepCopyValue recursively clones map[string]any and []any trees so that
// each goroutine gets its own mutable copy. gojq.normalizeNumbers mutates
// input maps in-place, so sharing a single DataSource across goroutines
// causes "concurrent map iteration and map write" panics.
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

	if len(work) < parallelThreshold {
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
	)
	for _, w := range work {
		wg.Add(1)
		dsCopy := deepCopyValue(opts.DataSource).(map[string]any)
		go func(w workItem) {
			defer wg.Done()
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
