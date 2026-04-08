package apiserver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"

	"go.datum.net/ufo/internal/registry/function"
	"go.datum.net/ufo/internal/registry/functionbuild"
	"go.datum.net/ufo/internal/registry/functionrevision"
	"go.datum.net/ufo/pkg/apis/functions/install"
	"go.datum.net/ufo/pkg/apis/functions/v1alpha1"
)

var (
	// Scheme defines the runtime type system for API object serialization.
	Scheme = runtime.NewScheme()
	// Codecs provides serializers for API objects.
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	install.Install(Scheme)

	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})

	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// Config combines generic and functions-specific configuration.
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
}

// FunctionsServer is the ufo aggregated API server.
type FunctionsServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
}

// CompletedConfig prevents incomplete configuration from being used.
type CompletedConfig struct {
	*completedConfig
}

// Complete validates and fills default values for the configuration.
func (cfg *Config) Complete() CompletedConfig {
	c := completedConfig{
		cfg.GenericConfig.Complete(),
	}
	return CompletedConfig{&c}
}

// New creates and initializes the FunctionsServer with storage and API groups.
func (c completedConfig) New() (*FunctionsServer, error) {
	genericServer, err := c.GenericConfig.New("ufo-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &FunctionsServer{
		GenericAPIServer: genericServer,
	}

	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(v1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)

	v1alpha1Storage := map[string]rest.Storage{}

	functionStorage, functionStatusStorage, err := function.NewStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, err
	}
	v1alpha1Storage["functions"] = functionStorage
	v1alpha1Storage["functions/status"] = functionStatusStorage

	functionRevisionStorage, functionRevisionStatusStorage, err := functionrevision.NewStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, err
	}
	v1alpha1Storage["functionrevisions"] = functionRevisionStorage
	v1alpha1Storage["functionrevisions/status"] = functionRevisionStatusStorage

	functionBuildStorage, functionBuildStatusStorage, err := functionbuild.NewStorage(Scheme, c.GenericConfig.RESTOptionsGetter)
	if err != nil {
		return nil, err
	}
	v1alpha1Storage["functionbuilds"] = functionBuildStorage
	v1alpha1Storage["functionbuilds/status"] = functionBuildStatusStorage

	apiGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = v1alpha1Storage

	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	return s, nil
}
