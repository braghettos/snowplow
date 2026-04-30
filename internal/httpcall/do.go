package httpcall

// The request-build body in this file mirrors the logic in
//
//	github.com/krateoplatformops/plumbing/http/request/request.go (v0.9.3)
//
// licensed under the MIT License (Copyright krateoplatformops contributors).
// We copy ~30 LOC because plumbing does not expose a `DoWithClient` entry
// point: the only injection seam is `HTTPClientForEndpoint`, which we
// replace with a cached client.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/http/util"
	"github.com/krateoplatformops/plumbing/ptr"
)

// Type-alias re-exports keep the migration diff at the call sites to a
// single import line.
type (
	RequestOptions = request.RequestOptions
	RequestInfo    = request.RequestInfo
)

const maxUnstructuredResponseTextBytes = 2048

// Do is a drop-in replacement for plumbing/http/request.Do. When the pool is
// enabled (default), the request runs on a cached *http.Client whose
// underlying *http.Transport reuses connections across calls keyed by
// endpoint identity. When SNOWPLOW_HTTP_POOL=off, AWS-auth is in use, the
// pool is full, or the endpoint is nil, this falls through to plumbing's
// per-call request.Do.
func Do(ctx context.Context, opts RequestOptions) *response.Status {
	if disabled || opts.Endpoint == nil {
		return request.Do(ctx, opts)
	}
	// AWS endpoints sign request headers at client-construction time inside
	// plumbing's awsAuthRoundTripper. Caching the client breaks per-request
	// signing: every subsequent request would carry the headers of the
	// first one. Until plumbing exposes a per-request signer, AWS bypasses
	// the pool.
	// TODO(httpcall): pool AWS once plumbing offers per-request signing.
	if opts.Endpoint.HasAwsAuth() {
		return request.Do(ctx, opts)
	}

	cli, ok := clientFor(opts.Endpoint)
	if !ok {
		// Pool full or construction failed: fall back to plumbing.
		return request.Do(ctx, opts)
	}
	return doWithClient(ctx, cli, opts)
}

// clientFor returns a pooled *http.Client for the given endpoint. The bool
// is false when the endpoint cannot be pooled (e.g. construction error or
// pool full); callers must fall back to plumbing in that case.
func clientFor(ep *endpoints.Endpoint) (*http.Client, bool) {
	key := endpointKey(ep)

	if v, ok := pool.Load(key); ok {
		return v.(*pooledClient).cli, true
	}

	// Build a fresh client. Delegate to plumbing for TLS / auth round-tripper
	// wiring, then replace the *http.Transport with our pooled one so we get
	// reasonable per-host idle caps.
	cli, err := request.HTTPClientForEndpoint(ep, nil)
	if err != nil || cli == nil {
		return nil, false
	}
	if tr, isHT := cli.Transport.(*http.Transport); isHT {
		applyPoolTransportSettings(tr)
	} else {
		// Wrapped transport (debug / auth round trippers). Walk the chain
		// looking for the underlying *http.Transport. This is best-effort:
		// if we can't find it, the client still works — it just inherits
		// plumbing's defaults.
		applyPoolToInnerTransport(cli.Transport)
	}

	entry := &pooledClient{cli: cli}
	if actual, loaded := pool.LoadOrStore(key, entry); loaded {
		return actual.(*pooledClient).cli, true
	}
	// Hard-cap check: if storing this entry pushed us over the cap, undo
	// and bypass. There is intentionally no eviction. If you hit this you
	// have endpoint-explosion in templates and that is a different bug.
	if poolSize.Add(1) > poolHardCap {
		pool.Delete(key)
		poolSize.Add(-1)
		return nil, false
	}
	return cli, true
}

// applyPoolTransportSettings tunes a fresh *http.Transport to our pool
// settings.
func applyPoolTransportSettings(tr *http.Transport) {
	tr.MaxIdleConns = poolMaxIdleConns
	tr.MaxIdleConnsPerHost = poolMaxIdleConnsPerHost
	tr.MaxConnsPerHost = poolMaxConnsPerHost
	tr.IdleConnTimeout = poolIdleConnTimeout
}

// applyPoolToInnerTransport walks a chain of round-tripper wrappers looking
// for an *http.Transport via reflection-free type assertions on common
// shapes. Best-effort: failure to find one is fine, the client still works.
func applyPoolToInnerTransport(rt http.RoundTripper) {
	type inner interface{ Inner() http.RoundTripper }
	for cur := rt; cur != nil; {
		if tr, ok := cur.(*http.Transport); ok {
			applyPoolTransportSettings(tr)
			return
		}
		if w, ok := cur.(inner); ok {
			cur = w.Inner()
			continue
		}
		// Plumbing's wrappers don't expose Inner(); they hold the
		// delegated round tripper in unexported fields. We can't reach
		// them without reflection, so we stop here. The transport will
		// use plumbing's defaults — still better than allocating fresh
		// transports per call (which is the leak we're fixing).
		return
	}
}

// endpointKey derives a stable identity hash for an endpoint that captures
// every input that could change Transport/RoundTripper behavior.
// Credentials are SHA-256 hashed before mixing in. The resulting hex digest
// is opaque and MUST NOT be logged.
func endpointKey(ep *endpoints.Endpoint) string {
	h := sha256.New()

	writeField(h, "url", ep.ServerURL)
	writeField(h, "proxy", ep.ProxyURL)
	writeField(h, "ca", ep.CertificateAuthorityData)
	writeField(h, "cert", ep.ClientCertificateData)
	writeField(h, "key", ep.ClientKeyData)
	if ep.Insecure {
		writeField(h, "insecure", "1")
	}

	switch {
	case ep.HasTokenAuth():
		t := sha256.Sum256([]byte(ep.Token))
		writeField(h, "auth", "token")
		h.Write(t[:])
	case ep.HasBasicAuth():
		bp := sha256.Sum256([]byte(ep.Username + "\x00" + ep.Password))
		writeField(h, "auth", "basic")
		h.Write(bp[:])
	case ep.HasAwsAuth():
		// Defensive only — Do() short-circuits AWS to plumbing.
		ak := sha256.Sum256([]byte(ep.AwsAccessKey + "\x00" + ep.AwsSecretKey))
		writeField(h, "auth", "aws")
		h.Write(ak[:])
		writeField(h, "aws-region", ep.AwsRegion)
		writeField(h, "aws-service", ep.AwsService)
	default:
		writeField(h, "auth", "none")
	}

	return hex.EncodeToString(h.Sum(nil))
}

// writeField mixes a labeled field into the hash with a 0x00 separator to
// prevent ambiguous concatenation across fields.
func writeField(h io.Writer, label, val string) {
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write([]byte(val))
	h.Write([]byte{0})
}

// doWithClient is a copy of plumbing v0.9.3 request.Do, minus the per-call
// HTTPClientForEndpoint allocation. It builds the *http.Request the same
// way plumbing does (including AWS header munging) and dispatches via the
// supplied (pooled) client wrapped in plumbing's RetryClient.
//
// Source attribution: github.com/krateoplatformops/plumbing v0.9.3, MIT.
func doWithClient(ctx context.Context, cli *http.Client, opts RequestOptions) *response.Status {
	uri := strings.TrimSuffix(opts.Endpoint.ServerURL, "/")
	if len(opts.Path) > 0 {
		uri = fmt.Sprintf("%s/%s", uri, strings.TrimPrefix(opts.Path, "/"))
	}

	u, err := url.Parse(uri)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}

	verb := ptr.Deref(opts.Verb, http.MethodGet)

	var body io.Reader
	if s := ptr.Deref(opts.Payload, ""); len(s) > 0 {
		body = strings.NewReader(s)
	}

	call, err := http.NewRequestWithContext(ctx, verb, u.String(), body)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}

	// Additional headers for AWS Signature 4 algorithm. Defensive: Do()
	// short-circuits AWS to plumbing's per-call path, so we never reach
	// this branch from production. Kept for parity with plumbing.
	if opts.Endpoint.HasAwsAuth() {
		headers, _, _, _, _, _ := request.ComputeAwsHeaders(opts.Endpoint, &opts.RequestInfo)
		opts.Headers = append(opts.Headers, headers...)
		opts.Headers = append(opts.Headers, xcontext.LabelKrateoTraceId+":"+xcontext.TraceId(ctx, true))
		for i := range opts.Headers {
			parts := strings.Split(opts.Headers[i], ":")
			opts.Headers[i] = strings.ToLower(strings.Trim(parts[0], " ")) + ":" + strings.Trim(parts[1], " ")
		}
		sort.Strings(opts.Headers)
	} else {
		call.Header.Set(xcontext.LabelKrateoTraceId, xcontext.TraceId(ctx, true))
	}

	if len(opts.Headers) > 0 {
		for _, el := range opts.Headers {
			idx := strings.Index(el, ":")
			if idx <= 0 {
				continue
			}
			key := el[:idx]
			val := strings.TrimSpace(el[idx+1:])
			call.Header.Set(key, val)
		}
	}

	retryCli := util.NewRetryClient(cli)

	respo, err := retryCli.Do(call)
	if err != nil {
		return response.New(http.StatusInternalServerError, err)
	}
	defer respo.Body.Close()

	statusOK := respo.StatusCode >= 200 && respo.StatusCode < 300
	if !statusOK {
		dat, err := io.ReadAll(io.LimitReader(respo.Body, maxUnstructuredResponseTextBytes))
		if err != nil {
			return response.New(http.StatusInternalServerError, err)
		}
		res := &response.Status{}
		if err := json.Unmarshal(dat, res); err != nil {
			res = response.New(respo.StatusCode, fmt.Errorf("%s", string(dat)))
			return res
		}
		return res
	}

	if ct := respo.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return response.New(http.StatusNotAcceptable, fmt.Errorf("content type %q is not allowed", ct))
	}

	if opts.ResponseHandler != nil {
		if err := opts.ResponseHandler(respo.Body); err != nil {
			return response.New(http.StatusInternalServerError, err)
		}
		return response.New(http.StatusOK, nil)
	}

	return response.New(http.StatusNoContent, nil)
}
