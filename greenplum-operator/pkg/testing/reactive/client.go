package reactive

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

type Client struct {
	testing.Fake
	delegate   client.Client
	restMapper meta.RESTMapper
}

var _ client.Client = &Client{}

func (r *Client) Scheme() *runtime.Scheme {
	return r.delegate.Scheme()
}

func (r *Client) RESTMapper() meta.RESTMapper {
	return r.restMapper
}

// Workaround for testing.ListAction missing GetKind(). It looks like an oversight.
type workaroundListAction interface {
	testing.ListAction
	GetKind() schema.GroupVersionKind
}

func NewClient(delegate client.Client) *Client {
	clientScheme := delegate.Scheme()
	gvs := clientScheme.PrioritizedVersionsAllGroups()
	restMapper := meta.NewDefaultRESTMapper(gvs)
	knownTypes := clientScheme.AllKnownTypes()
	for gvk := range knownTypes {
		restMapper.Add(gvk, meta.RESTScopeNamespace)
	}

	r := &Client{
		delegate:   delegate,
		restMapper: restMapper,
	}

	r.PrependReactor("*", "*", func(action testing.Action) (bool, runtime.Object, error) {
		ctx := context.TODO()
		switch action.GetVerb() {
		case "get":
			a := action.(testing.GetAction)
			key := types.NamespacedName{
				Name:      a.GetName(),
				Namespace: a.GetNamespace(),
			}
			obj := r.newNamedObject(r.kindForResource(a.GetResource()), a.GetNamespace(), a.GetName())
			err := r.delegate.Get(ctx, key, obj)
			return true, obj, err
		case "create":
			a := action.(testing.CreateAction)
			err := r.delegate.Create(ctx, a.GetObject().(client.Object))
			return true, nil, err
		case "delete":
			a := action.(testing.DeleteAction)
			obj := r.newNamedObject(r.kindForResource(a.GetResource()), a.GetNamespace(), a.GetName())
			err := r.delegate.Delete(ctx, obj)
			return true, nil, err
		case "update":
			a := action.(testing.UpdateAction)
			err := r.delegate.Update(ctx, a.GetObject().(client.Object))
			return true, nil, err
		case "patch":
			a := action.(testing.PatchAction)
			obj := r.newNamedObject(r.kindForResource(a.GetResource()), a.GetNamespace(), a.GetName())
			patch := client.RawPatch(a.GetPatchType(), a.GetPatch())
			err := r.delegate.Patch(ctx, obj, patch)
			return true, nil, err
		case "list":
			a := action.(workaroundListAction)
			obj := r.newObjectList(a.GetKind())
			err := r.delegate.List(ctx, obj,
				client.MatchingFieldsSelector{Selector: a.GetListRestrictions().Fields},
				client.MatchingLabelsSelector{Selector: a.GetListRestrictions().Labels},
				client.InNamespace(a.GetNamespace()),
			)
			return true, obj, err
		default:
			return true, nil, fmt.Errorf("unsupported action for verb %#v", action.GetVerb())
		}
	})

	return r
}

func (r *Client) gvrForObject(obj client.Object) schema.GroupVersionResource {
	defer GinkgoRecover()
	kinds, _, err := r.Scheme().ObjectKinds(obj)
	Expect(err).NotTo(HaveOccurred())
	Expect(kinds).To(HaveLen(1))
	gvk := kinds[0]

	rm, err := r.restMapper.RESTMapping(gvk.GroupKind())
	Expect(err).NotTo(HaveOccurred())
	gvr := rm.Resource

	return gvr
}

func (r *Client) kindForResource(resource schema.GroupVersionResource) schema.GroupVersionKind {
	defer GinkgoRecover()
	kind, err := r.restMapper.KindFor(resource)
	Expect(err).NotTo(HaveOccurred())
	return kind
}

func (r *Client) newNamedObject(kind schema.GroupVersionKind, namespace, name string) client.Object {
	defer GinkgoRecover()
	rObj := r.newRuntimeObject(kind)
	cObj, ok := rObj.(client.Object)
	Expect(ok).To(BeTrue(), "Expected object to implement client.Object. Does it implement metav1.Object?")
	cObj.SetNamespace(namespace)
	cObj.SetName(name)
	return cObj
}

func (r *Client) newObjectList(kind schema.GroupVersionKind) client.ObjectList {
	defer GinkgoRecover()
	rObj := r.newRuntimeObject(kind)
	cObj, ok := rObj.(client.ObjectList)
	Expect(ok).To(BeTrue(), "Expected object to implement client.ObjectList. Does it implement metav1.ListInterface?")
	return cObj
}

func (r *Client) newRuntimeObject(kind schema.GroupVersionKind) runtime.Object {
	defer GinkgoRecover()
	obj, err := r.Scheme().New(kind)
	Expect(err).NotTo(HaveOccurred())
	return obj
}

func (r *Client) populateGVK(obj client.Object) {
	defer GinkgoRecover()
	// Set GVK using reflection. Normally the apiserver would populate this, but we need it earlier.
	gvk, err := apiutil.GVKForObject(obj, r.Scheme())
	Expect(err).NotTo(HaveOccurred())
	obj.GetObjectKind().SetGroupVersionKind(gvk)
}

func (r *Client) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	action := testing.NewGetAction(r.gvrForObject(obj), key.Namespace, key.Name)
	retrievedObj, err := r.Invokes(action, nil)
	if err != nil {
		return err
	}
	return r.Scheme().Convert(retrievedObj, obj, nil)
}

func (r *Client) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	defer GinkgoRecover()

	listOpts := client.ListOptions{}
	listOpts.ApplyOptions(opts)

	listGvk, err := apiutil.GVKForObject(list, r.Scheme())
	if err != nil {
		return err
	}

	if !strings.HasSuffix(listGvk.Kind, "List") {
		return fmt.Errorf("non-list type %T (kind %q) passed as output", list, listGvk)
	}
	// we need the non-list GVK, so chop off the "List" from the end of the kind
	gvk := listGvk
	gvk.Kind = gvk.Kind[:len(gvk.Kind)-len("List")]

	gvr, _ := meta.UnsafeGuessKindToResource(gvk)

	action := testing.NewListAction(gvr, listGvk, listOpts.Namespace, *listOpts.AsListOptions())
	retrievedObj, err := r.Invokes(action, nil)
	if err != nil {
		return err
	}
	return r.Scheme().Convert(retrievedObj, list, nil)
}

func (r *Client) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	defer GinkgoRecover()
	Expect(opts).To(BeEmpty(), "we can't handle opts")
	object, err := meta.Accessor(obj)
	if err != nil {
		return errors.Wrap(err, "failed creating object")
	}

	r.populateGVK(obj)

	action := testing.NewCreateAction(r.gvrForObject(obj), object.GetNamespace(), obj)
	_, err = r.Invokes(action, nil)
	return err
}

func (r *Client) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	defer GinkgoRecover()

	// TODO: We are just dropping these options on the floor... this is the same thing
	//       that the controller-runtime fake client does, so it doesn't seem too unusual
	//       but is that really the right thing to do here?
	deleteOpts := client.DeleteOptions{}
	deleteOpts.ApplyOptions(opts)

	object, err := meta.Accessor(obj)
	if err != nil {
		return errors.Wrap(err, "failed deleting object")
	}

	action := testing.NewDeleteAction(r.gvrForObject(obj), object.GetNamespace(), object.GetName())
	_, err = r.Invokes(action, nil)
	return err
}

func (r *Client) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	panic("implement me")
}

func (r *Client) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	defer GinkgoRecover()
	Expect(opts).To(BeEmpty(), "we can't handle opts")
	object, err := meta.Accessor(obj)
	if err != nil {
		return errors.Wrap(err, "failed updating object")
	}

	r.populateGVK(obj)

	action := testing.NewUpdateAction(r.gvrForObject(obj), object.GetNamespace(), obj)
	_, err = r.Invokes(action, nil)
	return err
}

func (r *Client) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	defer GinkgoRecover()
	Expect(opts).To(BeEmpty(), "we can't handle opts")
	object, err := meta.Accessor(obj)
	if err != nil {
		return errors.Wrap(err, "failed patching object")
	}
	p, err := patch.Data(obj)
	if err != nil {
		return errors.Wrap(err, "failed patching object")
	}
	action := testing.NewPatchAction(r.gvrForObject(obj), object.GetNamespace(), object.GetName(), patch.Type(), p)
	_, err = r.Invokes(action, nil)
	return err
}

func (r *Client) Status() client.StatusWriter {
	return r
}
