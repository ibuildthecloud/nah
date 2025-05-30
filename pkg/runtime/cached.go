package runtime

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/obot-platform/nah/pkg/untriggered"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

const (
	cacheDuration = 10 * time.Second
)

type objectKey struct {
	gvk             schema.GroupVersionKind
	namespace, name string
}

type objectValue struct {
	Object   kclient.Object
	Inserted time.Time
}

type cacheClient struct {
	uncached kclient.WithWatch
	cached   kclient.Client

	recent     map[objectKey]objectValue
	recentLock sync.Mutex
}

func newer(oldRV, newRV string) bool {
	if len(oldRV) == len(newRV) {
		return oldRV < newRV
	}
	oldI, err := strconv.Atoi(oldRV)
	if err != nil {
		return true
	}
	newI, err := strconv.Atoi(newRV)
	if err != nil {
		return false
	}
	return oldI < newI
}

func newCacheClient(uncached kclient.WithWatch, cached kclient.Client) *cacheClient {
	return &cacheClient{
		uncached: uncached,
		cached:   cached,
		recent:   map[objectKey]objectValue{},
	}
}

func (c *cacheClient) startPurge(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(cacheDuration):
			}

			now := time.Now()
			c.recentLock.Lock()
			for k, v := range c.recent {
				if v.Inserted.Add(cacheDuration).Before(now) {
					delete(c.recent, k)
				}
			}
			c.recentLock.Unlock()
		}
	}()
}

func (c *cacheClient) deleteStore(obj kclient.Object) {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return
	}
	c.recentLock.Lock()
	delete(c.recent, objectKey{
		gvk:       gvk,
		namespace: obj.GetNamespace(),
		name:      obj.GetName(),
	})
	c.recentLock.Unlock()
}

func (c *cacheClient) store(obj kclient.Object) {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return
	}
	c.recentLock.Lock()
	c.recent[objectKey{
		gvk:       gvk,
		namespace: obj.GetNamespace(),
		name:      obj.GetName(),
	}] = objectValue{
		Object:   obj.DeepCopyObject().(kclient.Object),
		Inserted: time.Now(),
	}
	c.recentLock.Unlock()
}

func (c *cacheClient) Get(ctx context.Context, key kclient.ObjectKey, obj kclient.Object, opts ...kclient.GetOption) error {
	ctx, span := tracer.Start(ctx, "cachedGet")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.Get(ctx, key, obj, opts...)
		}
	}

	getErr := c.cached.Get(ctx, key, obj)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return getErr
	}

	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return errors.Join(getErr, err)
	}

	cacheKey := objectKey{
		gvk:       gvk,
		namespace: obj.GetNamespace(),
		name:      obj.GetName(),
	}

	c.recentLock.Lock()
	cachedObj, ok := c.recent[cacheKey]
	c.recentLock.Unlock()

	if apierrors.IsNotFound(getErr) {
		if ok {
			return CopyInto(obj, cachedObj.Object)
		}
		return getErr
	}

	if ok && newer(obj.GetResourceVersion(), cachedObj.Object.GetResourceVersion()) {
		return CopyInto(obj, cachedObj.Object)
	}

	return nil
}

func (c *cacheClient) List(ctx context.Context, list kclient.ObjectList, opts ...kclient.ListOption) error {
	ctx, span := tracer.Start(ctx, "cachedList")
	defer span.End()

	if u, ok := list.(*untriggered.HolderList); ok {
		list = u.ObjectList
		if u.IsUncached() {
			return c.uncached.List(ctx, list, opts...)
		}
	}
	return c.cached.List(ctx, list, opts...)
}

func (c *cacheClient) Create(ctx context.Context, obj kclient.Object, opts ...kclient.CreateOption) error {
	ctx, span := tracer.Start(ctx, "cachedCreate")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.Create(ctx, obj, opts...)
		}
	}
	err := c.cached.Create(ctx, obj, opts...)
	if err != nil {
		return err
	}
	c.store(obj)
	return nil
}

func (c *cacheClient) Delete(ctx context.Context, obj kclient.Object, opts ...kclient.DeleteOption) error {
	ctx, span := tracer.Start(ctx, "cachedDelete")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.Delete(ctx, obj, opts...)
		}
	}
	err := c.cached.Delete(ctx, obj, opts...)
	if err != nil {
		return err
	}
	c.deleteStore(obj)
	return nil
}

func (c *cacheClient) Update(ctx context.Context, obj kclient.Object, opts ...kclient.UpdateOption) error {
	ctx, span := tracer.Start(ctx, "cachedUpdate")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.Update(ctx, obj, opts...)
		}
	}
	err := c.cached.Update(ctx, obj, opts...)
	if err != nil {
		return err
	}
	c.store(obj)
	return nil
}

func (c *cacheClient) Patch(ctx context.Context, obj kclient.Object, patch kclient.Patch, opts ...kclient.PatchOption) error {
	ctx, span := tracer.Start(ctx, "cachedPatch")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.Patch(ctx, obj, patch, opts...)
		}
	}
	err := c.cached.Patch(ctx, obj, patch, opts...)
	if err != nil {
		return err
	}
	c.store(obj)
	return nil
}

func (c *cacheClient) DeleteAllOf(ctx context.Context, obj kclient.Object, opts ...kclient.DeleteAllOfOption) error {
	ctx, span := tracer.Start(ctx, "cachedDeleteAllOf")
	defer span.End()

	if u, ok := obj.(*untriggered.Holder); ok {
		obj = u.Object
		if u.IsUncached() {
			return c.uncached.DeleteAllOf(ctx, obj, opts...)
		}
	}
	return c.cached.DeleteAllOf(ctx, obj, opts...)
}

func (c *cacheClient) SubResource(subResource string) kclient.SubResourceClient {
	client := c.cached.SubResource(subResource)
	return &subResourceClient{
		c:      c,
		reader: client,
		writer: client,
	}
}

func (c *cacheClient) Watch(ctx context.Context, obj kclient.ObjectList, opts ...kclient.ListOption) (watch.Interface, error) {
	ctx, span := tracer.Start(ctx, "cachedWatch")
	defer span.End()

	return c.uncached.Watch(ctx, obj, opts...)
}

func (c *cacheClient) Status() kclient.StatusWriter {
	return &subResourceClient{
		c:      c,
		writer: c.cached.Status(),
	}
}

func (c *cacheClient) Scheme() *runtime.Scheme {
	return c.cached.Scheme()
}

func (c *cacheClient) RESTMapper() meta.RESTMapper {
	return c.cached.RESTMapper()
}

type subResourceClient struct {
	c      *cacheClient
	writer kclient.SubResourceWriter
	reader kclient.SubResourceReader
}

func (s *subResourceClient) Get(ctx context.Context, obj kclient.Object, subResource kclient.Object, opts ...kclient.SubResourceGetOption) error {
	ctx, span := tracer.Start(ctx, "subResource/get")
	defer span.End()

	return s.reader.Get(ctx, untriggered.Unwrap(obj).(kclient.Object), subResource, opts...)
}

func (s *subResourceClient) Create(ctx context.Context, obj kclient.Object, subResource kclient.Object, opts ...kclient.SubResourceCreateOption) error {
	ctx, span := tracer.Start(ctx, "subResource/create")
	defer span.End()

	return s.writer.Create(ctx, untriggered.Unwrap(obj).(kclient.Object), subResource, opts...)
}

func (s *subResourceClient) Update(ctx context.Context, obj kclient.Object, opts ...kclient.SubResourceUpdateOption) error {
	ctx, span := tracer.Start(ctx, "subResource/update")
	defer span.End()

	err := s.writer.Update(ctx, untriggered.Unwrap(obj).(kclient.Object), opts...)
	if err != nil {
		return err
	}
	s.c.store(obj)
	return nil
}

func (s *subResourceClient) Patch(ctx context.Context, obj kclient.Object, patch kclient.Patch, opts ...kclient.SubResourcePatchOption) error {
	ctx, span := tracer.Start(ctx, "subResource/patch")
	defer span.End()

	err := s.writer.Patch(ctx, untriggered.Unwrap(obj).(kclient.Object), patch, opts...)
	if err != nil {
		return err
	}
	s.c.store(obj)
	return nil
}
