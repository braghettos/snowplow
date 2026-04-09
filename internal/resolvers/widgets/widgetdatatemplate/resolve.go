package widgetdatatemplate

import (
	"context"
	"encoding/json"
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

// preNormalize walks the value tree and converts json.Number to int/float64,
// exactly as gojq's normalizeNumbers does. After this, gojq's normalizeNumbers
// becomes a no-op (all values are already native types), making the map safe
// to share read-only across goroutines for pure-read JQ queries.
func preNormalize(v any) any {
	switch val := v.(type) {
	case map[string]any:
		for k, x := range val {
			val[k] = preNormalize(x)
		}
		return val
	case []any:
		for i, x := range val {
			val[i] = preNormalize(x)
		}
		return val
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return int(i)
		}
		if f, err := val.Float64(); err == nil {
			return f
		}
		return val.String()
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

	// Pre-normalize the DataSource so gojq's normalizeNumbers is a no-op.
	// This makes the map safe to share across parallel goroutines for
	// pure-read JQ queries (selectors, filters, aggregations).
	preNormalize(opts.DataSource)

	if len(work) < parallelThreshold {
		for _, w := range work {
			if err := eval(w, opts.DataSource); err != nil {
				return nil, err
			}
		}
		return results, nil
	}

	// Run parallel JQ on the shared pre-normalized DataSource.
	// No deepCopy needed — normalizeNumbers is now a no-op, and
	// widget JQ expressions are pure reads (no assignment operators).
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for _, w := range work {
		wg.Add(1)
		go func(w workItem) {
			defer wg.Done()
			if err := eval(w, opts.DataSource); err != nil {
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
