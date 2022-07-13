package syncer

import (
	"fmt"

	jsonyaml "github.com/ghodss/yaml"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/config"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/patches"
	"github.com/loft-sh/vcluster-sdk/syncer"
	synccontext "github.com/loft-sh/vcluster-sdk/syncer/context"
	"github.com/loft-sh/vcluster-sdk/syncer/translator"
	"github.com/loft-sh/vcluster-sdk/translate"
	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func CreateFromVirtualSyncer(ctx *synccontext.RegisterContext, config *config.FromVirtualCluster) (syncer.Syncer, error) {
	obj := &unstructured.Unstructured{}
	obj.SetKind(config.Kind)
	obj.SetAPIVersion(config.ApiVersion)

	var err error
	var selector labels.Selector
	if config.Selector != nil && len(config.Selector.LabelSelector) > 0 {
		selector, err = metav1.LabelSelectorAsSelector(metav1.SetAsLabelSelector(config.Selector.LabelSelector))
		if err != nil {
			return nil, errors.Wrap(err, "parse label selector")
		}
	}

	return &fromVirtualController{
		NamespacedTranslator: translator.NewNamespacedTranslator(ctx, config.Kind+"-from-virtual-syncer", obj),

		config:   config,
		selector: selector,
	}, nil
}

type fromVirtualController struct {
	translator.NamespacedTranslator

	config   *config.FromVirtualCluster
	selector labels.Selector
}

// func Resource() client.Object

func (f *fromVirtualController) SyncDown(ctx *synccontext.SyncContext, vObj client.Object) (ctrl.Result, error) {
	// check if selector matches
	if !f.objectMatches(vObj) {
		return ctrl.Result{}, nil
	}

	// new obj
	newObj := f.TranslateMetadata(vObj)

	err := f.applyPatches(vObj, newObj)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply declared patches to %s %s/%s: %v", f.config.Kind, newObj.GetNamespace(), newObj.GetName(), err)
	}

	ctx.Log.Infof("create physical %s %s/%s", f.config.Kind, newObj.GetNamespace(), newObj.GetName())
	err = ctx.PhysicalClient.Create(ctx.Context, newObj)
	if err != nil {
		ctx.Log.Infof("error syncing %s %s/%s to physical cluster: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
		f.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (f *fromVirtualController) Sync(ctx *synccontext.SyncContext, pObj client.Object, vObj client.Object) (ctrl.Result, error) {
	if !f.objectMatches(vObj) {
		ctx.Log.Infof("delete physical %s %s/%s, because it is not used anymore", f.config.Kind, pObj.GetNamespace(), pObj.GetName())
		err := ctx.PhysicalClient.Delete(ctx.Context, pObj)
		if err != nil {
			ctx.Log.Infof("error deleting physical %s %s/%s in physical cluster: %v", f.config.Kind, pObj.GetNamespace(), pObj.GetName(), err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	if !equality.Semantic.DeepEqual(pObj, vObj) {
		// objects have changed
		ctx.Log.Infof("semantic difference between physical and virtual object")

		newObj := vObj.DeepCopyObject().(client.Object)
		err := f.applyReversePatches(ctx, pObj, newObj)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to apply declared patches to %s %s/%s: %v", f.config.Kind, newObj.GetNamespace(), newObj.GetName(), err)
		}

		err = ctx.VirtualClient.Patch(ctx.Context, vObj, client.MergeFrom(newObj))
		if err != nil {
			ctx.Log.Infof("error syncing %s %s/%s to virtual cluster: %v", f.config.Kind, newObj.GetNamespace(), newObj.GetName(), err)
			f.EventRecorder().Eventf(pObj, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
			return ctrl.Result{}, err
		}

	}

	return ctrl.Result{}, nil
}

func (f *fromVirtualController) objectMatches(obj client.Object) bool {
	return f.selector == nil || !f.selector.Matches(labels.Set(obj.GetLabels()))
}

func (f *fromVirtualController) applyPatches(vObj, pObj client.Object) error {
	yamlNode, err := patches.NewJSONNode(pObj)
	if err != nil {
		return errors.Wrap(err, "new json yaml node")
	}

	var otherYamlNode *yaml.Node
	if pObj != nil {
		otherYamlNode, err = patches.NewJSONNode(vObj)
		if err != nil {
			return errors.Wrap(err, "new json yaml node")
		}
	}

	err = patches.ApplyPatches(yamlNode, otherYamlNode, f.config.Patches, &virtualToHostNameResolver{namespace: vObj.GetNamespace()})
	if err != nil {
		return errors.Wrap(err, "error applying patches")
	}

	objYaml, err := yaml.Marshal(yamlNode)
	if err != nil {
		return errors.Wrap(err, "marshal yaml")
	}

	err = jsonyaml.Unmarshal(objYaml, pObj)
	if err != nil {
		return errors.Wrap(err, "convert object")
	}

	return nil
}

func (f *fromVirtualController) applyReversePatches(ctx *synccontext.SyncContext, pObj, vObj client.Object) error {
	yamlNode, err := patches.NewJSONNode(vObj)
	if err != nil {
		return errors.Wrap(err, "new json yaml node")
	}

	var otherYamlNode *yaml.Node
	if pObj != nil {
		otherYamlNode, err = patches.NewJSONNode(pObj)
		if err != nil {
			return errors.Wrap(err, "new json yaml node")
		}
	}

	// TODO: pass HostToVirtualNameResolver instead

	ctx.Log.Infof("applying reverse patches")
	err = patches.ApplyPatches(yamlNode, otherYamlNode, f.config.ReversePatches, &virtualToHostNameResolver{namespace: vObj.GetNamespace()})
	if err != nil {
		return errors.Wrap(err, "error applying patches")
	}

	objYaml, err := yaml.Marshal(yamlNode)
	if err != nil {
		return errors.Wrap(err, "marshal yaml")
	}

	err = jsonyaml.Unmarshal(objYaml, vObj)
	if err != nil {
		return errors.Wrap(err, "convert object")
	}

	return nil
}

type virtualToHostNameResolver struct {
	namespace string
}

func (n *virtualToHostNameResolver) TranslateName(name string) (string, error) {
	return translate.PhysicalName(name, n.namespace), nil
}
