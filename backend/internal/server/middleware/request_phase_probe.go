package middleware

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RequestPhaseProbeContextKey 探针对象在 gin.Context 里的 key。
const RequestPhaseProbeContextKey = "request_phase_probe"

// requestPhaseProbeDefaultAlertAfter 请求超过这么久仍未完成 → 打一条告警(阶段+耗时)。
const requestPhaseProbeDefaultAlertAfter = 60 * time.Second

// requestPhaseProbeMaxTimeline 时间线条目上限(转发循环可能多次打阶段,封顶防膨胀)。
const requestPhaseProbeMaxTimeline = 64

// RequestPhaseProbe 请求阶段探针:记录一次网关请求依次经过的阶段(鉴权/限速/审计/审核/
// 转发子阶段…)与进入时刻,并给请求体套一层计数 reader(已读字节、首/末次读时间、EOF、
// 读错误)。请求超过 alertAfter 仍未完成时,由独立定时器打一条 Warn(当前阶段、该阶段已
// 耗时、完整时间线、请求体读取进度);请求完成时若总耗时 ≥ alertAfter(或已告过警),再补
// 一条带最终状态码 / ctx 错误的时间线。正常快请求零日志。
//
// 用途:定位「中央记 499 context canceled 127s、却没有审核/转发/心跳日志」这类卡在
// 转发之前的慢请求 —— 到底是请求体上传没读完(隧道/CF 侧),还是鉴权/Redis/审核在等。
//
// 必须挂在 RequestBodyLimit 之后(包住 MaxBytesReader),其它中间件之前;各中间件用
// Phase() 包装或在内部调 SetRequestPhase() 打点。定时器回调不触碰 gin.Context(只读
// 探针内部的原子/带锁字段),避免与请求 goroutine 竞争。
func RequestPhaseProbe(alertAfter time.Duration) gin.HandlerFunc {
	if alertAfter <= 0 {
		alertAfter = requestPhaseProbeDefaultAlertAfter
	}
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}
		p := newRequestPhaseProbe(c, alertAfter)
		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			pb := &probeBody{ReadCloser: c.Request.Body}
			p.body = pb
			c.Request.Body = pb
		}
		c.Set(RequestPhaseProbeContextKey, p)
		p.timer = time.AfterFunc(alertAfter, p.alert)
		// defer:内层 panic(由外层 Recovery 接住)也能收尾,定时器不泄漏。
		defer p.finish(c)
		c.Next()
	}
}

// Phase 把一个中间件/处理函数包成「进入即打阶段」:探针记下 name 与进入时刻,再调用 h。
// 探针未挂载(如非网关路由)时只是透传。
func Phase(name string, h gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		SetRequestPhase(c, name)
		h(c)
	}
}

// SetRequestPhase 在中间件/处理函数内部打细分阶段(如 edge_forward.read_body)。
// 探针未挂载时为 no-op。
func SetRequestPhase(c *gin.Context, name string) {
	if c == nil {
		return
	}
	if p := requestPhaseProbeFrom(c); p != nil {
		p.setPhase(name)
	}
}

func requestPhaseProbeFrom(c *gin.Context) *requestPhaseProbe {
	v, ok := c.Get(RequestPhaseProbeContextKey)
	if !ok {
		return nil
	}
	p, _ := v.(*requestPhaseProbe)
	return p
}

type phaseEntry struct {
	name string
	at   time.Time
}

type requestPhaseProbe struct {
	start         time.Time
	alertAfter    time.Duration
	logger        *zap.Logger
	method        string
	path          string
	contentLength int64
	body          *probeBody
	timer         *time.Timer

	mu       sync.Mutex
	timeline []phaseEntry
	done     bool
	alerted  bool
}

func newRequestPhaseProbe(c *gin.Context, alertAfter time.Duration) *requestPhaseProbe {
	now := time.Now()
	p := &requestPhaseProbe{
		start:         now,
		alertAfter:    alertAfter,
		logger:        logger.FromContext(c.Request.Context()).With(zap.String("component", "request_phase_probe")),
		method:        c.Request.Method,
		path:          c.Request.URL.Path,
		contentLength: c.Request.ContentLength,
		timeline:      []phaseEntry{{name: "entry", at: now}},
	}
	return p
}

func (p *requestPhaseProbe) setPhase(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	if len(p.timeline) >= requestPhaseProbeMaxTimeline {
		// 封顶后只覆盖最后一条,保证「当前阶段」仍准确。
		p.timeline[len(p.timeline)-1] = phaseEntry{name: name, at: time.Now()}
		return
	}
	p.timeline = append(p.timeline, phaseEntry{name: name, at: time.Now()})
}

// alert 定时器回调:请求超过 alertAfter 仍未完成。只读探针自身状态,不碰 gin.Context。
func (p *requestPhaseProbe) alert() {
	p.mu.Lock()
	if p.done {
		p.mu.Unlock()
		return
	}
	p.alerted = true
	fields := p.fieldsLocked(time.Now())
	p.mu.Unlock()
	p.logger.Warn("request_phase_probe: 请求超过阈值仍未完成", fields...)
}

// finish 请求收尾(请求 goroutine 上调用):停定时器;慢请求补一条带状态码/ctx 错误的时间线。
func (p *requestPhaseProbe) finish(c *gin.Context) {
	if p.timer != nil {
		p.timer.Stop()
	}
	now := time.Now()
	p.mu.Lock()
	p.done = true
	slow := p.alerted || now.Sub(p.start) >= p.alertAfter
	var fields []zap.Field
	if slow {
		fields = p.fieldsLocked(now)
	}
	p.mu.Unlock()
	if !slow {
		return
	}
	fields = append(fields, zap.Int("status", c.Writer.Status()))
	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			fields = append(fields, zap.String("ctx_err", err.Error()))
		}
	}
	p.logger.Warn("request_phase_probe: 慢请求完成,阶段时间线", fields...)
}

// fieldsLocked 组装日志字段;调用方持有 p.mu。
func (p *requestPhaseProbe) fieldsLocked(now time.Time) []zap.Field {
	cur := p.timeline[len(p.timeline)-1]
	fields := []zap.Field{
		zap.String("method", p.method),
		zap.String("path", p.path),
		zap.Int64("elapsed_ms", now.Sub(p.start).Milliseconds()),
		zap.String("phase", cur.name),
		zap.Int64("phase_elapsed_ms", now.Sub(cur.at).Milliseconds()),
		zap.String("timeline", p.timelineStringLocked()),
		zap.Int64("content_length", p.contentLength),
	}
	if p.body != nil {
		fields = append(fields, p.body.fields(p.start)...)
	}
	return fields
}

// timelineStringLocked 形如 "entry@0ms > api_key_auth@3ms > edge_forward.read_body@5ms"。
func (p *requestPhaseProbe) timelineStringLocked() string {
	var b strings.Builder
	for i, e := range p.timeline {
		if i > 0 {
			b.WriteString(" > ")
		}
		b.WriteString(e.name)
		b.WriteByte('@')
		b.WriteString(strconv.FormatInt(e.at.Sub(p.start).Milliseconds(), 10))
		b.WriteString("ms")
	}
	return b.String()
}

// probeBody 请求体计数 reader:已读字节、首/末次 Read 返回时刻、是否读到 EOF、首个读错误。
// 用来区分「请求体根本没传完(上传卡住)」与「传完了卡在后面的处理」。
type probeBody struct {
	io.ReadCloser
	bytesRead   atomic.Int64
	firstReadAt atomic.Int64 // unix nano;0=尚未读过
	lastReadAt  atomic.Int64
	eof         atomic.Bool
	errMu       sync.Mutex
	readErr     error
}

func (b *probeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	now := time.Now().UnixNano()
	b.firstReadAt.CompareAndSwap(0, now)
	b.lastReadAt.Store(now)
	if n > 0 {
		b.bytesRead.Add(int64(n))
	}
	if err != nil {
		if err == io.EOF {
			b.eof.Store(true)
		} else {
			b.errMu.Lock()
			if b.readErr == nil {
				b.readErr = err
			}
			b.errMu.Unlock()
		}
	}
	return n, err
}

func (b *probeBody) fields(start time.Time) []zap.Field {
	fields := []zap.Field{
		zap.Int64("body_bytes_read", b.bytesRead.Load()),
		zap.Bool("body_eof", b.eof.Load()),
	}
	if first := b.firstReadAt.Load(); first > 0 {
		fields = append(fields,
			zap.Int64("body_first_read_ms", (first-start.UnixNano())/int64(time.Millisecond)),
			zap.Int64("body_last_read_ms", (b.lastReadAt.Load()-start.UnixNano())/int64(time.Millisecond)),
		)
	}
	b.errMu.Lock()
	err := b.readErr
	b.errMu.Unlock()
	if err != nil {
		fields = append(fields, zap.String("body_err", err.Error()))
	}
	return fields
}
