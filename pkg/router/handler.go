package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/locker"
	"github.com/obot-platform/nah/pkg/backend"
	"github.com/obot-platform/nah/pkg/log"
	"github.com/obot-platform/nah/pkg/merr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/exp/maps"
	"golang.org/x/time/rate"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	TriggerPrefix = "_t "
	ReplayPrefix  = "_r "
)

var tracer = otel.Tracer("nah/router")

type HandlerSet struct {
	ctx      context.Context
	name     string
	scheme   *runtime.Scheme
	backend  backend.Backend
	handlers handlers
	triggers triggers
	save     save
	onError  ErrorHandler

	watchingLock sync.Mutex
	watching     map[schema.GroupVersionKind]bool
	locker       locker.Locker

	limiterLock sync.Mutex
	limiters    map[limiterKey]*rate.Limiter
	waiting     map[limiterKey]struct{}
}

type limiterKey struct {
	key string
	gvk schema.GroupVersionKind
}

func NewHandlerSet(name string, scheme *runtime.Scheme, backend backend.Backend) *HandlerSet {
	hs := &HandlerSet{
		name:    name,
		scheme:  scheme,
		backend: backend,
		handlers: handlers{
			handlers: map[schema.GroupVersionKind][]handler{},
		},
		triggers: triggers{
			trigger:     backend,
			triggerLock: sync.NewCond(&sync.Mutex{}),
			gvkLookup:   backend,
			scheme:      scheme,
		},
		save: save{
			cache:  backend,
			client: backend,
		},
		watching: map[schema.GroupVersionKind]bool{},
	}
	hs.triggers.watcher = hs
	return hs
}

func (m *HandlerSet) Start(ctx context.Context) error {
	if m.ctx == nil {
		m.ctx = ctx
	}
	if err := m.WatchGVK(m.handlers.GVKs()...); err != nil {
		return err
	}
	return m.backend.Start(ctx)
}

func (m *HandlerSet) Preload(ctx context.Context) error {
	if m.ctx == nil {
		m.ctx = ctx
	}
	if err := m.WatchGVK(m.handlers.GVKs()...); err != nil {
		return err
	}
	return m.backend.Preload(ctx)
}

func toObject(obj runtime.Object) kclient.Object {
	if obj == nil {
		return nil
	}
	// yep panic if it's not this interface
	return obj.DeepCopyObject().(kclient.Object)
}

type triggerRegistry struct {
	gvk     schema.GroupVersionKind
	gvks    map[schema.GroupVersionKind]bool
	key     string
	trigger *triggers
}

func (t *triggerRegistry) WatchingGVKs() []schema.GroupVersionKind {
	return maps.Keys(t.gvks)

}
func (t *triggerRegistry) Watch(obj runtime.Object, namespace, name string, sel labels.Selector, fields fields.Selector) error {
	gvk, ok, err := t.trigger.Register(t.gvk, t.key, obj, namespace, name, sel, fields)
	if err != nil {
		return err
	}
	if ok {
		t.gvks[gvk] = true
	}
	return nil
}

func (m *HandlerSet) newRequestResponse(ctx context.Context, gvk schema.GroupVersionKind, key string, runtimeObject runtime.Object, trigger bool) (Request, *response, error) {
	var (
		obj = toObject(runtimeObject)
	)

	ns, name, ok := strings.Cut(key, "/")
	if !ok {
		name = key
		ns = ""
	}

	triggerRegistry := &triggerRegistry{
		gvk:     gvk,
		key:     key,
		trigger: &m.triggers,
		gvks:    map[schema.GroupVersionKind]bool{},
	}

	resp := response{
		registry: triggerRegistry,
	}

	req := Request{
		FromTrigger: trigger,
		Client: &client{
			backend: m.backend,
			reader: reader{
				scheme:   m.scheme,
				client:   m.backend,
				registry: triggerRegistry,
			},
			writer: writer{
				client:   m.backend,
				registry: triggerRegistry,
			},
			status: status{
				client:   m.backend,
				registry: triggerRegistry,
			},
		},
		Ctx:       ctx,
		GVK:       gvk,
		Object:    obj,
		Namespace: ns,
		Name:      name,
		Key:       key,
	}

	return req, &resp, nil
}

func (m *HandlerSet) AddHandler(name string, objType kclient.Object, handler Handler) {
	gvk, err := m.backend.GVKForObject(objType, m.scheme)
	if err != nil {
		panic(fmt.Sprintf("scheme does not know gvk for %T", objType))
	}
	m.handlers.AddHandler(name, gvk, handler)
}

func (m *HandlerSet) WatchGVK(gvks ...schema.GroupVersionKind) error {
	var watchErrs []error
	m.watchingLock.Lock()
	for _, gvk := range gvks {
		if m.watching[gvk] {
			continue
		}
		if err := m.backend.Watcher(m.ctx, gvk, m.name, m.onChange); err == nil {
			m.watching[gvk] = true
		} else {
			watchErrs = append(watchErrs, err)
		}
	}
	m.watchingLock.Unlock()
	return merr.NewErrors(watchErrs...)
}

func (m *HandlerSet) checkDelay(gvk schema.GroupVersionKind, key string) bool {
	m.limiterLock.Lock()
	defer m.limiterLock.Unlock()
	lKey := limiterKey{key: key, gvk: gvk}

	if _, ok := m.waiting[lKey]; ok {
		return false
	}

	limit, ok := m.limiters[lKey]
	if !ok {
		// Limit to once every 15 seconds with a burst of 10. This limits the
		// overall rate at which we can process a key regardless of the key
		// source (change event, trigger, error re-enqueue)
		limit = rate.NewLimiter(rate.Limit(1.0/5), 10)
		if m.limiters == nil {
			m.limiters = map[limiterKey]*rate.Limiter{}
		}
		m.limiters[lKey] = limit
	}

	delay := limit.Reserve().Delay()
	if delay > 0 {
		if m.waiting == nil {
			m.waiting = map[limiterKey]struct{}{}
		}
		m.waiting[lKey] = struct{}{}
		go func() {
			log.Debugf("Backing off [%s] [%s] for %s", key, gvk, delay)
			time.Sleep(delay)
			m.limiterLock.Lock()
			defer m.limiterLock.Unlock()
			delete(m.waiting, lKey)
			_ = m.backend.Trigger(m.ctx, gvk, ReplayPrefix+key, 0)
		}()
		return false
	}

	return true
}

func (m *HandlerSet) forgetBackoff(gvk schema.GroupVersionKind, key string) {
	m.limiterLock.Lock()
	defer m.limiterLock.Unlock()
	delete(m.limiters, limiterKey{key: key, gvk: gvk})
}

func (m *HandlerSet) onChange(ctx context.Context, gvk schema.GroupVersionKind, key string, runtimeObject runtime.Object) (runtime.Object, error) {
	ctx, span := tracer.Start(ctx, "onChange", trace.WithAttributes(attribute.String("key", key)), trace.WithAttributes(attribute.String("gvk", gvk.String())))
	defer span.End()

	fromTrigger := false
	fromReplay := false
	if strings.HasPrefix(key, TriggerPrefix) {
		fromTrigger = true
		key = strings.TrimPrefix(key, TriggerPrefix)
	}
	if strings.HasPrefix(key, ReplayPrefix) {
		fromTrigger = false
		fromReplay = true
		key = strings.TrimPrefix(key, ReplayPrefix)
	}

	if !fromReplay && !fromTrigger {
		// Process delay have key has been reassigned from the TriggerPrefix
		if !m.checkDelay(gvk, key) {
			return runtimeObject, nil
		}
	}

	obj, err := m.scheme.New(gvk)
	if err != nil {
		return nil, err
	}

	ns, name, ok := strings.Cut(key, "/")
	if !ok {
		name = key
		ns = ""
	}

	lockKey := gvk.Kind + " " + key
	m.locker.Lock(lockKey)
	defer func() { _ = m.locker.Unlock(lockKey) }()

	err = m.backend.Get(ctx, kclient.ObjectKey{Name: name, Namespace: ns}, obj.(kclient.Object))
	if err == nil {
		runtimeObject = obj
	} else if !apierror.IsNotFound(err) {
		return nil, err
	}

	if runtimeObject == nil {
		m.forgetBackoff(gvk, key)
	}

	return m.handle(ctx, gvk, key, runtimeObject, fromTrigger)
}

func (m *HandlerSet) handleError(req Request, resp Response, err error) error {
	if m.onError != nil {
		return m.onError(req, resp, err)
	}
	return err
}

func (m *HandlerSet) handle(ctx context.Context, gvk schema.GroupVersionKind, key string, unmodifiedObject runtime.Object, trigger bool) (runtime.Object, error) {
	req, resp, err := m.newRequestResponse(ctx, gvk, key, unmodifiedObject, trigger)
	if err != nil {
		return nil, err
	}

	handles := m.handlers.Handles(req)
	if handles {
		if req.FromTrigger {
			log.Debugf("Handling trigger [%s/%s] [%v]", req.Namespace, req.Name, req.GVK)
		} else {
			log.Debugf("Handling [%s/%s] [%v]", req.Namespace, req.Name, req.GVK)
		}

		if err := m.handlers.Handle(req, resp); err != nil {
			if err := m.handleError(req, resp, err); err != nil {
				return nil, err
			}
		}
	}

	_, span := tracer.Start(ctx, "trigger", trace.WithAttributes(attribute.String("key", key), attribute.String("gvk", gvk.String()), attribute.Bool("unregister", unmodifiedObject == nil)))
	if unmodifiedObject == nil {
		// A nil object here means that the object was deleted, so unregister the triggers
		m.triggers.UnregisterAndTrigger(req)
	} else if !req.FromTrigger {
		m.triggers.Trigger(req)
	}
	span.End()

	if handles {
		newObj, err := m.save.save(unmodifiedObject, req)
		if err != nil {
			if err := m.handleError(req, resp, err); err != nil {
				return nil, err
			}
		}
		req.Object = newObj

		if resp.delay > 0 {
			if err := m.backend.Trigger(ctx, gvk, key, resp.delay); err != nil {
				return nil, err
			}
		}
	}

	return req.Object, m.handleError(req, resp, err)
}

type ResponseAttributes struct {
	attr map[string]any
}

func (r *ResponseAttributes) Attributes() map[string]any {
	if r.attr == nil {
		r.attr = map[string]any{}
	}
	return r.attr
}

type response struct {
	ResponseAttributes

	delay    time.Duration
	registry TriggerRegistry
}

func (r *response) RetryAfter(delay time.Duration) {
	if r.delay == 0 || delay < r.delay {
		r.delay = delay
	}
}

func (r *response) WatchingGVKs() []schema.GroupVersionKind {
	return r.registry.WatchingGVKs()
}
