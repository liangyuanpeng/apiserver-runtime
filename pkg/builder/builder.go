/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package builder

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/registry/generic"
	regsitryrest "k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/apiserver"
	"sigs.k8s.io/apiserver-runtime/internal/sample-apiserver/pkg/cmd/server"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource/resourcerest"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/resource/resourcestrategy"
	"sigs.k8s.io/apiserver-runtime/pkg/builder/rest"
)

// APIServer builds an apiserver to server Kubernetes resources and sub resources.
var APIServer = &Server{
	storage: map[schema.GroupResource]*singletonProvider{},
}

// Server builds a new apiserver for a single API group
type Server struct {
	errs                 []error
	storage              map[schema.GroupResource]*singletonProvider
	groupVersions        map[schema.GroupVersion]bool
	orderedGroupVersions []schema.GroupVersion
	schemes              []*runtime.Scheme
	schemeBuilder        runtime.SchemeBuilder
}

// WithOpenAPIDefinitions registers resource OpenAPI definitions generated by openapi-gen.
//
//    export K8sAPIS=k8s.io/apimachinery/pkg/api/resource,\
//      k8s.io/apimachinery/pkg/apis/meta/v1,\
//      k8s.io/apimachinery/pkg/runtime,\
//      k8s.io/apimachinery/pkg/version
//    export MY_APIS=my-go-pkg/pkg/apis/my-group/my-version
//    export OPENAPI=my-go-pkg/pkg/generated/openapi
//    openapi-gen --input-dirs $K8SAPIS,$MY_APIS --output-package $OPENAPI \
//      -O zz_generated.openapi --output-base ../../.. --go-header-file ./hack/boilerplate.go.txt
func (a *Server) WithOpenAPIDefinitions(
	name, version string, openAPI openapicommon.GetOpenAPIDefinitions) *Server {
	server.SetOpenAPIDefinitions(name, version, openAPI)
	return a
}

// WithAdditionalSchemeInstallers registers functions to install additional functions or resources into the Scheme.
// This can be used to manually registering defaulting functions, conversion functions, or resource types, rather
// than registering them automatically by implementing the corresponding interfaces on the resources.
func (a *Server) WithAdditionalSchemeInstallers(fns ...func(*runtime.Scheme) error) *Server {
	a.schemeBuilder.Register(fns...)
	return a
}

// WithAdditionalSchemesToBuild will add types and functions to these Schemes in addition to the
// apiserver.Scheme.
// This can be used to register the resource types, defaulting functions, and conversion functions
// with additional Scheme's.
func (a *Server) WithAdditionalSchemesToBuild(s ...*runtime.Scheme) *Server {
	a.schemes = append(a.schemes, s...)
	return a
}

// WithResource registers the resource with the apiserver.
//
// If no versions of this GroupResource have already been registered, a new default handler will be registered.
// If the object implements rest.Getter, rest.Updater or rest.Creator then the provided object itself will be
// used as the rest handler for the resource type.
//
// If no versions of this GroupResource have already been registered and the object does NOT implement the rest
// interfaces, then a new etcd backed storage will be created for the object and used as the handler.
// The storage will use a DefaultStrategy, which delegates functions to the object if the object implements
// interfaces defined in the "apiserver-runtime/pkg/builder/rest" package.  Otherwise it will provide a default
// behavior.
//
// WithResource will automatically register the "status" subresource if the object implements the
// resource.StatusGetSetter interface.
//
// WithResource will automatically register version-specific defaulting for this GroupVersionResource
// if the object implements the resource.Defaulter interface.
//
// WithResource automatically adds the object and its list type to the known types.  If the object also declares itself
// as the storage version, the object and its list type will be added as storage versions to the SchemeBuilder as well.
// The storage version is the version accepted by the handler.
//
// If another version of the object's GroupResource has already been registered, then the resource will use the
// handler already registered for that version of the GroupResource.  Objects for this version will be converted
// to the object version which the handler accepts before the handler is invoked.
func (a *Server) WithResource(obj resource.Object) *Server {
	gvr := obj.GetGroupVersionResource()
	a.schemeBuilder.Register(resource.AddToScheme(obj))

	// reuse the storage if this resource has already been registered
	if s, found := a.storage[gvr.GroupResource()]; found {
		_ = a.forGroupVersionResource(gvr, obj, s.Get)
		return a
	}

	// If the type implements it's own storage, then use that
	switch s := obj.(type) {
	case resourcerest.Creator:
		return a.forGroupVersionResource(gvr, obj, rest.StaticHandlerProvider{Storage: s.(regsitryrest.Storage)}.Get)
	case resourcerest.Updater:
		return a.forGroupVersionResource(gvr, obj, rest.StaticHandlerProvider{Storage: s.(regsitryrest.Storage)}.Get)
	case resourcerest.Getter:
		return a.forGroupVersionResource(gvr, obj, rest.StaticHandlerProvider{Storage: s.(regsitryrest.Storage)}.Get)
	case resourcerest.Lister:
		return a.forGroupVersionResource(gvr, obj, rest.StaticHandlerProvider{Storage: s.(regsitryrest.Storage)}.Get)
	}

	_ = a.forGroupVersionResource(gvr, obj, rest.New(obj))

	// automatically create status subresource if the object implements the status interface
	if sgs, ok := obj.(resource.ObjectWithStatusSubResource); ok {
		st := gvr.GroupVersion().WithResource(gvr.Resource + "/status")
		if s, found := a.storage[st.GroupResource()]; found {
			_ = a.forGroupVersionResource(st, obj, s.Get)
		} else {
			_, _, _, sp := rest.NewStatus(sgs)
			_ = a.forGroupVersionResource(st, obj, sp)
		}
	}
	return a
}

// WithResourceAndStrategy registers the resource with the apiserver creating a new etcd backed storage
// for the GroupResource using the provided strategy.  In most cases callers should instead use WithResource
// and implement the interfaces defined in "apiserver-runtime/pkg/builder/rest" to control the Strategy.
//
// Note: WithResourceAndHandler should never be called after the GroupResource has already been registered with
// another version.
func (a *Server) WithResourceAndStrategy(obj resource.Object, strategy rest.Strategy) *Server {
	gvr := obj.GetGroupVersionResource()
	a.schemeBuilder.Register(resource.AddToScheme(obj))

	_ = a.forGroupVersionResource(gvr, obj, rest.NewWithStrategy(obj, strategy))

	// automatically create status subresource if the object implements the status interface
	if _, ok := obj.(resource.ObjectWithStatusSubResource); ok {
		st := gvr.GroupVersion().WithResource(gvr.Resource + "/status")
		_ = a.forGroupVersionResource(st, obj, rest.NewStatusWithStrategy(obj, strategy))
	}
	return a
}

// WithResourceAndHandler registers a request handler for the resource rather than the default
// etcd backed storage.
//
// Note: WithResourceAndHandler should never be called after the GroupResource has already been registered with
// another version.
//
// Note: WithResourceAndHandler will NOT register the "status" subresource for the resource object.
func (a *Server) WithResourceAndHandler(obj resource.Object, sp rest.ResourceHandlerProvider) *Server {
	gvr := obj.GetGroupVersionResource()
	a.schemeBuilder.Register(resource.AddToScheme(obj))
	return a.forGroupVersionResource(gvr, obj, sp)
}

// WithResourceAndStorage registers the resource with the apiserver, applying fn to the storage for the resource
// before completing it.
//
// May be used to change low-level storage configuration or swap out the storage backend to something other than
// etcd.
//
// Note: WithResourceAndHandler should never be called after the GroupResource has already been registered with
// another version.
func (a *Server) WithResourceAndStorage(obj resource.Object, fn rest.StoreFn) *Server {
	gvr := obj.GetGroupVersionResource()
	a.schemeBuilder.Register(resource.AddToScheme(obj))

	_ = a.forGroupVersionResource(gvr, obj, rest.NewWithFn(obj, fn))

	// automatically create status subresource if the object implements the status interface
	if _, ok := obj.(resource.ObjectWithStatusSubResource); ok {
		st := gvr.GroupVersion().WithResource(gvr.Resource + "/status")
		_ = a.forGroupVersionResource(st, obj, rest.NewStatusWithFn(obj, fn))
	}

	return a
}

// forGroupVersionResource manually registers storage for a specific resource or subresource version.
func (a *Server) forGroupVersionResource(
	gvr schema.GroupVersionResource, obj runtime.Object, sp rest.ResourceHandlerProvider) *Server {
	// register the group version
	a.withGroupVersions(gvr.GroupVersion())

	// TODO: make sure folks don't register multiple storage instance for the same group-resource
	// don't replace the existing instance otherwise it will chain wrapped singletonProviders when
	// fetching from the map before calling this function
	if _, found := a.storage[gvr.GroupResource()]; !found {
		a.storage[gvr.GroupResource()] = &singletonProvider{Provider: sp}
	}

	// add the defaulting function for this version to the scheme
	if _, ok := obj.(resourcestrategy.Defaulter); ok {
		apiserver.Scheme.AddTypeDefaultingFunc(obj, func(obj interface{}) {
			obj.(resourcestrategy.Defaulter).Default()
		})
	}

	// add the API with its storage
	apiserver.APIs[gvr] = sp
	return a
}

// WithSubResource registers a subresource with the apiserver under an existing resource.
// Subresource can be used to  implement interfaces which may be implemented by multiple resources -- e.g. "scale".
//
// Note: WithSubResource does NOT register the request or parent with the SchemeBuilder.  If they were not registered
// through a WithResource call, then this must be done manually with WithAdditionalSchemeInstallers.
func (a *Server) WithSubResource(
	parent resource.Object, subResourcePath string, request runtime.Object) *Server {
	gvr := parent.GetGroupVersionResource()
	gvr.Resource = gvr.Resource + "/" + subResourcePath

	// reuse the storage if this resource has already been registered
	if s, found := a.storage[gvr.GroupResource()]; found {
		_ = a.forGroupVersionResource(gvr, request, s.Get)
	} else {
		a.errs = append(a.errs, fmt.Errorf(
			"subresources must be registered with a strategy or handler the first time they are registered"))
	}
	return a
}

// WithSubResourceAndStrategy registers a subresource with the apiserver under an existing resource.
// Subresource can be used to  implement interfaces which may be implemented by multiple resources -- e.g. "scale".
//
// Note: WithSubResource does NOT register the request or parent with the SchemeBuilder.  If they were not registered
// through a WithResource call, then this must be done manually with WithAdditionalSchemeInstallers.
func (a *Server) WithSubResourceAndStrategy(
	parent resource.Object, subResourcePath string, request resource.Object, strategy rest.Strategy) *Server {
	gvr := parent.GetGroupVersionResource()
	gvr.Resource = gvr.Resource + "/" + subResourcePath
	return a.forGroupVersionResource(gvr, request, rest.NewWithStrategy(request, strategy))
}

// WithSubResourceAndHandler registers a request handler for the subresource rather than the default
// etcd backed storage.
//
// Note: WithSubResource does NOT register the request or parent with the SchemeBuilder.  If they were not registered
// through a WithResource call, then this must be done manually with WithAdditionalSchemeInstallers.
func (a *Server) WithSubResourceAndHandler(
	parent resource.Object, subResourcePath string, request runtime.Object, sp rest.ResourceHandlerProvider) *Server {
	gvr := parent.GetGroupVersionResource()
	// add the subresource path
	gvr.Resource = gvr.Resource + "/" + subResourcePath
	return a.forGroupVersionResource(gvr, request, sp)
}

// WithSchemeInstallers registers functions to install resource types into the Scheme.
func (a *Server) withGroupVersions(versions ...schema.GroupVersion) *Server {
	if a.groupVersions == nil {
		a.groupVersions = map[schema.GroupVersion]bool{}
	}
	for _, gv := range versions {
		if _, found := a.groupVersions[gv]; found {
			continue
		}
		a.groupVersions[gv] = true
		a.orderedGroupVersions = append(a.orderedGroupVersions, gv)
	}
	return a
}

// DisableDelegateAuth disables delegated authentication and authorization
func (a *Server) DisableDelegateAuth() *Server {
	server.ServerOptionsFns = append(server.ServerOptionsFns, func(o *ServerOptions) *ServerOptions {
		o.RecommendedOptions.Authentication.RemoteKubeConfigFileOptional = true
		o.RecommendedOptions.Authorization.RemoteKubeConfigFileOptional = true
		return o
	})
	return a
}

// WithOptionsFns sets functions to customize the ServerOptions used to create the apiserver
func (a *Server) WithOptionsFns(fns ...func(*ServerOptions) *ServerOptions) *Server {
	server.ServerOptionsFns = append(server.ServerOptionsFns, fns...)
	return a
}

// WithServerFns sets functions to customize the GenericAPIServer
func (a *Server) WithServerFns(fns ...func(server *GenericAPIServer) *GenericAPIServer) *Server {
	apiserver.GenericAPIServerFns = append(apiserver.GenericAPIServerFns, fns...)
	return a
}

// Build returns a Command used to run the apiserver
func (a *Server) Build() (*Command, error) {
	a.schemes = append(a.schemes, apiserver.Scheme)
	a.schemeBuilder.Register(
		func(scheme *runtime.Scheme) error {
			groupVersions := make(map[string]sets.String)
			for gvr := range apiserver.APIs {
				if groupVersions[gvr.Group] == nil {
					groupVersions[gvr.Group] = sets.NewString()
				}
				groupVersions[gvr.Group].Insert(gvr.Version)
			}
			for g, versions := range groupVersions {
				gvs := []schema.GroupVersion{}
				for _, v := range versions.List() {
					gvs = append(gvs, schema.GroupVersion{
						Group:   g,
						Version: v,
					})
				}
				err := scheme.SetVersionPriority(gvs...)
				if err != nil {
					return err
				}
			}
			for i := range a.orderedGroupVersions {
				metav1.AddToGroupVersion(scheme, a.orderedGroupVersions[i])
			}
			return nil
		},
	)
	for i := range a.schemes {
		if err := a.schemeBuilder.AddToScheme(a.schemes[i]); err != nil {
			panic(err)
		}
	}

	if len(a.errs) != 0 {
		return nil, errs{list: a.errs}
	}
	o := server.NewWardleServerOptions(os.Stdout, os.Stderr, a.orderedGroupVersions...)
	cmd := server.NewCommandStartServer(o, genericapiserver.SetupSignalHandler())
	cmd.Flags().AddGoFlagSet(flag.CommandLine)
	return cmd, nil
}

// Execute builds and executes the apiserver Command.
func (a *Server) Execute() error {
	cmd, err := a.Build()
	if err != nil {
		return err
	}
	return cmd.Execute()
}

// singletonProvider ensures different versions of the same resource share storage
type singletonProvider struct {
	sync.Once
	Provider rest.ResourceHandlerProvider
	storage  regsitryrest.Storage
	err      error
}

func (s *singletonProvider) Get(
	scheme *runtime.Scheme, optsGetter generic.RESTOptionsGetter) (regsitryrest.Storage, error) {
	s.Once.Do(func() {
		s.storage, s.err = s.Provider(scheme, optsGetter)
	})
	return s.storage, s.err
}

type errs struct {
	list []error
}

func (e errs) Error() string {
	msgs := []string{fmt.Sprintf("%d errors: ", len(e.list))}
	for i := range e.list {
		msgs = append(msgs, e.list[i].Error())
	}
	return strings.Join(msgs, "\n")
}
