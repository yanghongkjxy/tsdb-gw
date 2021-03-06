package api

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/metrictank/stats"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	otLog "github.com/opentracing/opentracing-go/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/raintank/tsdb-gw/api/models"
	"github.com/raintank/tsdb-gw/auth"
	"github.com/raintank/tsdb-gw/usage"
	log "github.com/sirupsen/logrus"
	"gopkg.in/macaron.v1"
)

var (
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "cortex_gw",
		Name:      "request_duration_seconds",
		Help:      "Time (in seconds) spent serving HTTP requests.",
		Buckets:   prometheus.ExponentialBuckets(.05, 2, 10),
	}, []string{"method", "route", "status_code"})
)

func init() {
	prometheus.MustRegister(requestDuration)
}

type TracingResponseWriter struct {
	macaron.ResponseWriter
	errBody []byte // the body in case it is an error
}

func (rw *TracingResponseWriter) Write(b []byte) (int, error) {
	if rw.ResponseWriter.Status() >= 400 {
		rw.errBody = make([]byte, len(b))
		copy(rw.errBody, b)
	}
	return rw.ResponseWriter.Write(b)
}

func GetContextHandler() macaron.Handler {
	return func(c *macaron.Context) {
		ctx := &models.Context{
			Context: c,
			User:    &auth.User{},
		}
		c.Map(ctx)
	}
}

func RequireAdmin() macaron.Handler {
	return func(ctx *models.Context) {
		if !ctx.IsAdmin {
			ctx.JSON(403, "Permision denied")
		}
	}
}

func (a *Api) Auth() macaron.Handler {
	return func(ctx *models.Context) {
		username, key, ok := ctx.Req.BasicAuth()
		if !ok {
			// no basicAuth, but we also need to check for a Bearer Token
			header := ctx.Req.Header.Get("Authorization")
			parts := strings.SplitN(header, " ", 2)
			if len(parts) == 2 && parts[0] == "Bearer" {
				key = parts[1]
				username = "api_key"
			}
		}

		if key == "" {
			log.Debugf("no key specified")
			ctx.JSON(401, "Unauthorized")
			return
		}

		user, err := a.authPlugin.Auth(username, key)
		if err != nil {
			if err == auth.ErrInvalidCredentials || err == auth.ErrInvalidOrgId || err == auth.ErrInvalidInstanceID {
				ctx.JSON(401, err.Error())
				return
			}
			log.Errorf("failed to perform authentication: %q", err.Error())
			ctx.JSON(500, err.Error())
			return
		}

		// allow admin users to impersonate other orgs.
		if user.IsAdmin {
			header := ctx.Req.Header.Get("X-Tsdb-Org")
			if header != "" {
				orgId, err := strconv.ParseInt(header, 10, 64)
				if err == nil && orgId != 0 {
					user.ID = int(orgId)
				}
			}
		}
		ctx.User = user
	}
}

func (a *Api) DDAuth() macaron.Handler {
	return func(ctx *models.Context) {
		var key string
		var username string

		header := ctx.Req.Header.Get("Dd-Api-Key")
		parts := strings.SplitN(header, ":", 2)
		if len(parts) == 1 {
			key = parts[0]
			username = "api_key"
		} else if len(parts) == 2 && parts[1] == "" {
			key = parts[1]
			username = "api_key"
		} else {
			key = parts[1]
			username = parts[0]
		}

		if key == "" {
			log.Debugf("no key specified")
			ctx.JSON(401, "Unauthorized")
			return
		}

		user, err := a.authPlugin.Auth(username, key)
		if err != nil {
			if err == auth.ErrInvalidCredentials || err == auth.ErrInvalidOrgId || err == auth.ErrInvalidInstanceID {
				ctx.JSON(401, err.Error())
				return
			}
			log.Errorf("failed to perform authentication: %q", err.Error())
			ctx.JSON(500, err.Error())
			return
		}

		// allow admin users to impersonate other orgs.
		if user.IsAdmin {
			header := ctx.Req.Header.Get("X-Tsdb-Org")
			if header != "" {
				orgId, err := strconv.ParseInt(header, 10, 64)
				if err == nil && orgId != 0 {
					user.ID = int(orgId)
				}
			}
		}
		ctx.User = user
	}
}

type requestStats struct {
	sync.Mutex
	responseCounts    map[string]map[int]*stats.CounterRate32
	latencyHistograms map[string]*stats.LatencyHistogram15s32
	sizeMeters        map[string]*stats.Meter32
}

func (r *requestStats) PathStatusCount(ctx *models.Context, path string, status int) {
	metricKey := fmt.Sprintf("api.request.%s.status.%d", path, status)
	r.Lock()
	p, ok := r.responseCounts[path]
	if !ok {
		p = make(map[int]*stats.CounterRate32)
		r.responseCounts[path] = p
	}
	c, ok := p[status]
	if !ok {
		c = stats.NewCounterRate32(metricKey)
		p[status] = c
	}
	r.Unlock()
	c.Inc()
	usage.LogRequest(ctx.ID, metricKey)
}

func (r *requestStats) PathLatency(ctx *models.Context, path string, dur time.Duration) {
	r.Lock()
	p, ok := r.latencyHistograms[path]
	if !ok {
		p = stats.NewLatencyHistogram15s32(fmt.Sprintf("api.request.%s", path))
		r.latencyHistograms[path] = p
	}
	r.Unlock()
	p.Value(dur)
}

func (r *requestStats) PathSize(ctx *models.Context, path string, size int) {
	r.Lock()
	p, ok := r.sizeMeters[path]
	if !ok {
		p = stats.NewMeter32(fmt.Sprintf("api.request.%s.size", path), false)
		r.sizeMeters[path] = p
	}
	r.Unlock()
	p.Value(size)
}

// RequestStats returns a middleware that tracks request metrics.
func RequestStats() macaron.Handler {
	stats := requestStats{
		responseCounts:    make(map[string]map[int]*stats.CounterRate32),
		latencyHistograms: make(map[string]*stats.LatencyHistogram15s32),
		sizeMeters:        make(map[string]*stats.Meter32),
	}

	return func(ctx *models.Context) {
		start := time.Now()
		rw := ctx.Resp.(macaron.ResponseWriter)
		// call next handler. This will return after all handlers
		// have completed and the request has been sent.
		ctx.Next()
		status := rw.Status()
		path := pathSlug(ctx.Req.URL.Path)
		stats.PathStatusCount(ctx, path, status)
		stats.PathLatency(ctx, path, time.Since(start))
		// only record the request size if the request succeeded.
		if status < 300 {
			stats.PathSize(ctx, path, rw.Size())
		}
	}
}

func (a *Api) PromStats(handler string) macaron.Handler {
	return func(ctx *models.Context) {
		start := time.Now()
		rw := ctx.Resp.(macaron.ResponseWriter)
		// call next handler. This will return after all handlers
		// have completed and the request has been sent.
		ctx.Next()

		status := strconv.Itoa(rw.Status())
		took := time.Since(start)
		method := ctx.Req.Method

		requestDuration.WithLabelValues(method, handler, status).Observe(took.Seconds())
	}
}

func pathSlug(p string) string {
	slug := strings.TrimPrefix(path.Clean(p), "/")
	if slug == "" {
		slug = "root"
	}
	return strings.Replace(strings.Replace(slug, "/", "_", -1), ".", "_", -1)
}

func Tracer(componentName string) macaron.Handler {
	return func(macCtx *macaron.Context) {
		tracer := opentracing.GlobalTracer()
		path := pathSlug(macCtx.Req.URL.Path)
		spanCtx, _ := tracer.Extract(opentracing.HTTPHeaders, opentracing.HTTPHeadersCarrier(macCtx.Req.Header))
		span := tracer.StartSpan("HTTP "+macCtx.Req.Method+" "+path, ext.RPCServerOption(spanCtx))

		ext.HTTPMethod.Set(span, macCtx.Req.Method)
		ext.HTTPUrl.Set(span, macCtx.Req.URL.String())
		ext.Component.Set(span, componentName+"/api")

		macCtx.Req = macaron.Request{
			Request: macCtx.Req.WithContext(opentracing.ContextWithSpan(macCtx.Req.Context(), span)),
		}
		macCtx.Resp = &TracingResponseWriter{
			ResponseWriter: macCtx.Resp,
		}
		macCtx.MapTo(macCtx.Resp, (*http.ResponseWriter)(nil))

		rw := macCtx.Resp.(*TracingResponseWriter)
		// call next handler. This will return after all handlers
		// have completed and the request has been sent.
		macCtx.Next()
		status := rw.Status()
		ext.HTTPStatusCode.Set(span, uint16(status))
		if status >= 200 && status < 300 {
			span.SetTag("http.size", rw.Size())
		}
		if status >= 400 {
			span.LogFields(otLog.Error(errors.New(string(rw.errBody))))
			if status >= 500 {
				ext.Error.Set(span, true)
			}
		}
		span.Finish()
	}
}
