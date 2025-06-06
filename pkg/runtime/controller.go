package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/nah/pkg/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgocache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const maxTimeout2min = 2 * time.Minute

type Handler interface {
	OnChange(ctx context.Context, key string, obj runtime.Object) error
}

type ResourceVersionGetter interface {
	GetResourceVersion() string
}

type HandlerFunc func(key string, obj runtime.Object) error

func (h HandlerFunc) OnChange(key string, obj runtime.Object) error {
	return h(key, obj)
}

type Controller interface {
	Enqueue(namespace, name string)
	EnqueueAfter(namespace, name string, delay time.Duration)
	EnqueueKey(key string)
	EnqueueKeyAfter(key string, delay time.Duration)
	Cache() (cache.Cache, error)
	Start(ctx context.Context, workers int) error
}

type controller struct {
	startLock sync.Mutex

	name         string
	workqueues   []workqueue.TypedRateLimitingInterface[any]
	rateLimiter  workqueue.TypedRateLimiter[any]
	informer     cache.Informer
	handler      Handler
	gvk          schema.GroupVersionKind
	startKeys    []startKey
	started      bool
	registration clientgocache.ResourceEventHandlerRegistration
	obj          runtime.Object
	cache        cache.Cache
	splitter     WorkerQueueSplitter
}

type startKey struct {
	key   string
	after time.Duration
}

type Options struct {
	RateLimiter   workqueue.TypedRateLimiter[any]
	QueueSplitter WorkerQueueSplitter
}

type WorkerQueueSplitter interface {
	Queues() int
	Split(key string) int
}

type singleWorkerQueueSplitter struct{}

func (*singleWorkerQueueSplitter) Queues() int {
	return 1
}

func (*singleWorkerQueueSplitter) Split(string) int {
	return 0
}

func New(ctx context.Context, gvk schema.GroupVersionKind, scheme *runtime.Scheme, cache cache.Cache, handler Handler, opts *Options) (Controller, error) {
	opts = applyDefaultOptions(opts)

	obj, err := newObject(scheme, gvk)
	if err != nil {
		return nil, err
	}

	informer, err := cache.GetInformerForKind(ctx, gvk)
	if err != nil {
		return nil, err
	}

	controller := &controller{
		gvk:         gvk,
		name:        gvk.String(),
		handler:     handler,
		cache:       cache,
		obj:         obj,
		rateLimiter: opts.RateLimiter,
		informer:    informer,
		splitter:    opts.QueueSplitter,
	}

	return controller, nil
}

func newObject(scheme *runtime.Scheme, gvk schema.GroupVersionKind) (runtime.Object, error) {
	obj, err := scheme.New(gvk)
	if runtime.IsNotRegisteredError(err) {
		return &unstructured.Unstructured{}, nil
	}
	return obj, err
}

func applyDefaultOptions(opts *Options) *Options {
	var newOpts Options
	if opts != nil {
		newOpts = *opts
	}
	if newOpts.RateLimiter == nil {
		newOpts.RateLimiter = workqueue.NewTypedMaxOfRateLimiter(
			workqueue.NewTypedItemFastSlowRateLimiter[any](time.Millisecond, maxTimeout2min, 30),
			workqueue.NewTypedItemExponentialFailureRateLimiter[any](5*time.Millisecond, 30*time.Second),
		)
	}
	if newOpts.QueueSplitter == nil {
		newOpts.QueueSplitter = (*singleWorkerQueueSplitter)(nil)
	}
	return &newOpts
}

func (c *controller) Cache() (cache.Cache, error) {
	return c.cache, nil
}

func (c *controller) GroupVersionKind() schema.GroupVersionKind {
	return c.gvk
}

func (c *controller) run(ctx context.Context, workers int) {
	defer func() {
		_ = c.informer.RemoveEventHandler(c.registration)
	}()

	c.startLock.Lock()
	// we have to defer queue creation until we have a stopCh available because a workqueue
	// will create a goroutine under the hood.  It we instantiate a workqueue we must have
	// a mechanism to Shutdown it down.  Without the stopCh we don't know when to shutdown
	// the queue and release the goroutine
	c.workqueues = make([]workqueue.TypedRateLimitingInterface[any], c.splitter.Queues())
	for i := range c.workqueues {
		c.workqueues[i] = workqueue.NewTypedRateLimitingQueueWithConfig(c.rateLimiter, workqueue.TypedRateLimitingQueueConfig[any]{Name: fmt.Sprintf("%s-%d", c.name, i)})
	}
	for _, start := range c.startKeys {
		if start.after == 0 {
			c.workqueues[c.splitter.Split(start.key)].Add(start.key)
		} else {
			c.workqueues[c.splitter.Split(start.key)].AddAfter(start.key, start.after)
		}
	}
	c.startKeys = nil
	c.startLock.Unlock()

	defer utilruntime.HandleCrash()

	// Start the informer factories to begin populating the informer caches
	log.Infof("Starting %s controller", c.name)

	c.runWorkers(ctx, workers)

	c.startLock.Lock()
	defer c.startLock.Unlock()
	c.started = false
	log.Infof("Shutting down %s workers", c.name)
}

func (c *controller) Start(ctx context.Context, workers int) error {
	ctx, span := tracer.Start(ctx, "controllerStart", trace.WithAttributes(
		attribute.String("gvk", c.gvk.String()),
		attribute.Int("workers", workers),
	))
	defer span.End()

	c.startLock.Lock()
	defer c.startLock.Unlock()

	if c.started {
		return nil
	}

	if c.informer == nil {
		informer, err := c.cache.GetInformerForKind(ctx, c.gvk)
		if err != nil {
			return err
		}
		if sii, ok := informer.(clientgocache.SharedIndexInformer); ok {
			c.informer = sii
		} else {
			return fmt.Errorf("expecting cache.SharedIndexInformer but got %T", informer)
		}
	}

	if c.registration == nil {
		registration, err := c.informer.AddEventHandler(clientgocache.ResourceEventHandlerFuncs{
			AddFunc: c.handleObject,
			UpdateFunc: func(old, new any) {
				c.handleObject(new)
			},
			DeleteFunc: c.handleObject,
		})
		if err != nil {
			return err
		}
		c.registration = registration
	}

	if !c.informer.HasSynced() {
		go func() {
			_ = c.cache.Start(ctx)
		}()
	}

	span.AddEvent("waiting for caches to sync")
	if ok := clientgocache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	span.AddEvent("starting workers")
	go c.run(ctx, workers)
	c.started = true
	return nil
}

func (c *controller) runWorkers(ctx context.Context, workers int) {
	wait := sync.WaitGroup{}
	workers = workers / len(c.workqueues)
	if workers == 0 {
		workers = 1
	}

	defer func() {
		defer wait.Wait()
	}()

	for _, queue := range c.workqueues {
		go func() {
			// This channel acts as a semaphore to limit the number of concurrent
			// work items handled by this controller.
			running := make(chan struct{}, workers)
			defer close(running)

			for {
				obj, shutdown := queue.Get()
				if shutdown {
					return
				}

				// Acquire from the semaphore
				running <- struct{}{}

				if queue.ShuttingDown() {
					// If we acquired after the workers were shutdown,
					// then drop this object and return instead of trying to add to the wait group, which will panic.
					return
				}

				wait.Add(1)

				go func() {
					defer func() {
						// Release to the semaphore
						<-running
						wait.Done()
					}()

					if err := c.processSingleItem(ctx, queue, obj); err != nil {
						if !strings.Contains(err.Error(), "please apply your changes to the latest version and try again") {
							log.Errorf("%v", err)
						}
					}
				}()
			}
		}()
	}

	<-ctx.Done()
	for i := range c.workqueues {
		c.workqueues[i].ShutDown()
	}
}

func (c *controller) processSingleItem(ctx context.Context, queue workqueue.TypedRateLimitingInterface[any], obj any) error {
	// Create a new root span for processing items.
	ctx, span := tracer.Start(ctx, "processSingleItem", trace.WithNewRoot(), trace.WithAttributes(
		attribute.String("gvk", c.gvk.String()),
		attribute.String("key", fmt.Sprintf("%v", obj)),
	))
	defer span.End()

	var (
		key string
		ok  bool
	)

	defer queue.Done(obj)

	if key, ok = obj.(string); !ok {
		queue.Forget(obj)
		log.Errorf("expected string in workqueue but got %#v", obj)
		return nil
	}
	if err := c.syncHandler(ctx, key); err != nil {
		queue.AddRateLimited(key)
		return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
	}

	queue.Forget(obj)
	return nil
}

func isSpecialKey(key string) bool {
	// This matches "_t " and "_r " prefixes
	return len(key) > 2 && key[0] == '_' && key[2] == ' '
}

func (c *controller) syncHandler(ctx context.Context, key string) error {
	if isSpecialKey(key) {
		return c.handler.OnChange(ctx, key, nil)
	}

	ns, name := KeyParse(key)
	obj := c.obj.DeepCopyObject().(kclient.Object)
	err := c.cache.Get(ctx, kclient.ObjectKey{
		Name:      name,
		Namespace: ns,
	}, obj)
	if apierror.IsNotFound(err) {
		return c.handler.OnChange(ctx, key, nil)
	} else if err != nil {
		return err
	}

	return c.handler.OnChange(ctx, key, obj.(runtime.Object))
}

func (c *controller) EnqueueKey(key string) {
	c.EnqueueKeyAfter(key, 0)
}

func (c *controller) EnqueueKeyAfter(key string, after time.Duration) {
	c.startLock.Lock()
	defer c.startLock.Unlock()

	if c.workqueues == nil {
		c.startKeys = append(c.startKeys, startKey{key: key, after: after})
	} else {
		c.workqueues[c.splitter.Split(key)].AddAfter(key, after)
	}
}

func (c *controller) Enqueue(namespace, name string) {
	c.EnqueueAfter(namespace, name, 0)
}

func (c *controller) EnqueueAfter(namespace, name string, duration time.Duration) {
	key := keyFunc(namespace, name)

	c.startLock.Lock()
	defer c.startLock.Unlock()

	if c.workqueues == nil {
		c.startKeys = append(c.startKeys, startKey{key: key, after: duration})
	} else {
		c.workqueues[c.splitter.Split(key)].AddAfter(key, duration)
	}
}

func KeyParse(key string) (namespace string, name string) {
	special, key, ok := strings.Cut(key, " ")
	if !ok {
		key = special
	}

	namespace, name, ok = strings.Cut(key, "/")
	if !ok {
		name = namespace
		namespace = ""
	}
	return
}

func keyFunc(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func (c *controller) enqueue(obj any) {
	var key string
	var err error
	if key, err = clientgocache.MetaNamespaceKeyFunc(obj); err != nil {
		log.Errorf("%v", err)
		return
	}
	c.startLock.Lock()
	if c.workqueues == nil {
		c.startKeys = append(c.startKeys, startKey{key: key})
	} else {
		c.workqueues[c.splitter.Split(key)].Add(key)
	}
	c.startLock.Unlock()
}

func (c *controller) handleObject(obj any) {
	if _, ok := obj.(metav1.Object); !ok {
		tombstone, ok := obj.(clientgocache.DeletedFinalStateUnknown)
		if !ok {
			log.Errorf("error decoding object, invalid type")
			return
		}
		newObj, ok := tombstone.Obj.(metav1.Object)
		if !ok {
			log.Errorf("error decoding object tombstone, invalid type")
			return
		}
		obj = newObj
	}
	c.enqueue(obj)
}
