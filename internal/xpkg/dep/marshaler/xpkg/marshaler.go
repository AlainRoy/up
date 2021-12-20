// Copyright 2021 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xpkg

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	"github.com/spf13/afero/tarfs"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	v1ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1beta1ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"

	"github.com/crossplane/crossplane-runtime/pkg/parser"
	xpmetav1 "github.com/crossplane/crossplane/apis/pkg/meta/v1"

	xpv1ext "github.com/crossplane/crossplane/apis/apiextensions/v1"
	xpv1beta1ext "github.com/crossplane/crossplane/apis/apiextensions/v1beta1"
	"github.com/crossplane/crossplane/apis/pkg/v1beta1"

	"github.com/upbound/up/internal/xpkg"
	xpkgparser "github.com/upbound/up/internal/xpkg/parser"
)

const (
	errFailedToParsePkgYaml         = "failed to parse package yaml"
	errLintPackage                  = "failed to lint package"
	errOpenPackageStream            = "failed to open package stream file"
	errFaileToAcquireDigest         = "failed to pull digest from image"
	errFailedToConvertMetaToPackage = "failed to convert meta to package"
	errInvalidPath                  = "invalid path provided for package lookup"
	errNotExactlyOneMeta            = "not exactly one package meta type"
	errObjectNotKnownType           = "object is not a known type"

	digestPrefix = "sha256:"
	dockerhub    = "index.docker.io"
)

// Marshaler represents a xpkg Marshaler
type Marshaler struct {
	p PackageParser
}

// NewMarshaler returns a new Marshaler
func NewMarshaler(opts ...MarshalerOption) (*Marshaler, error) {
	r := &Marshaler{}
	p, err := xpkgparser.New()
	if err != nil {
		return nil, err
	}

	r.p = p

	for _, o := range opts {
		o(r)
	}

	return r, nil
}

// MarshalerOption modifies the xpkg Marshaler
type MarshalerOption func(*Marshaler)

// WithParser modifies the Marshaler by setting the supplied PackageParser as
// the Resolver's parser.
func WithParser(p PackageParser) MarshalerOption {
	return func(r *Marshaler) {
		r.p = p
	}
}

// FromImage takes a registry, version, and name strings and their corresponding
// v1.Image and returns a ParsedPackage for consumption by upstream callers.
func (r *Marshaler) FromImage(reg, repo, ver string, i v1.Image) (*ParsedPackage, error) {
	digest, err := i.Digest()
	if err != nil {
		return nil, errors.Wrap(err, errFaileToAcquireDigest)
	}

	reader := mutate.Extract(i)
	fs := tarfs.New(tar.NewReader(reader))
	pkgYaml, err := fs.Open(xpkg.StreamFile)
	if err != nil {
		return nil, errors.Wrap(err, errOpenPackageStream)
	}

	pkg, err := r.parse(pkgYaml)
	if err != nil {
		return nil, err
	}

	return finalizePkg(reg, repo, ver, digest.String(), pkg)
}

// FromDir takes an afero.Fs, path to a directory, registry reference, and name
// returns a ParsedPackage based on the directories contents for consumption by
// upstream callers.
func (r *Marshaler) FromDir(fs afero.Fs, path, reg, repo string) (*ParsedPackage, error) {
	parts := strings.Split(path, "@")
	if len(parts) != 2 {
		return nil, errors.New(errInvalidPath)
	}

	var digest string
	reader, err := parser.NewFsReadCloser(fs, path, parser.SkipDirs(), skipDigest(&digest))
	if err != nil {
		return nil, err
	}

	pkg, err := r.parse(reader)
	if err != nil {
		return nil, err
	}

	return finalizePkg(reg, repo, parts[1], digest, pkg)
}

func (r *Marshaler) parse(reader io.ReadCloser) (*ParsedPackage, error) {
	// parse package.yaml
	pkg, err := r.p.Parse(context.Background(), reader)
	if err != nil {
		return nil, errors.Wrap(err, errFailedToParsePkgYaml)
	}

	metas := pkg.GetMeta()
	if len(metas) != 1 {
		return nil, errors.New(errNotExactlyOneMeta)
	}

	meta := metas[0]
	var linter parser.Linter
	var pkgType v1beta1.PackageType
	if meta.GetObjectKind().GroupVersionKind().Kind == xpmetav1.ConfigurationKind {
		linter = xpkg.NewConfigurationLinter()
		pkgType = v1beta1.ConfigurationPackageType
	} else {
		linter = xpkg.NewProviderLinter()
		pkgType = v1beta1.ProviderPackageType
	}
	if err := linter.Lint(pkg); err != nil {
		return nil, errors.Wrap(err, errLintPackage)
	}

	return &ParsedPackage{
		MetaObj: meta,
		Objs:    pkg.GetObjects(),
		PType:   pkgType,
	}, nil
}

func finalizePkg(reg, repo, ver, digest string, pkg *ParsedPackage) (*ParsedPackage, error) { // nolint:gocyclo
	deps, err := determineDeps(pkg.MetaObj)
	if err != nil {
		return nil, err
	}

	// generate GVK -> validators map for the package
	v := map[schema.GroupVersionKind]*validate.SchemaValidator{}

	for _, o := range pkg.Objects() {
		switch rd := o.(type) {
		case *v1beta1ext.CustomResourceDefinition:
			if err := validatorsFromV1Beta1CRD(rd, v); err != nil {
				return nil, err
			}
		case *v1ext.CustomResourceDefinition:
			if err := validatorsFromV1CRD(rd, v); err != nil {
				return nil, err
			}
		case *xpv1beta1ext.CompositeResourceDefinition:
			if err := validatorsFromV1Beta1XRD(rd, v); err != nil {
				return nil, err
			}
		case *xpv1ext.CompositeResourceDefinition:
			if err := validatorsFromV1XRD(rd, v); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New(errObjectNotKnownType)
		}
	}

	pkg.Deps = deps
	pkg.GVKtoV = v
	pkg.DepName = derivePkgName(reg, repo)
	pkg.Reg = reg
	pkg.SHA = digest
	pkg.Ver = ver

	return pkg, nil
}

// derivePkgName returns the package name that we'd expect to see in a meta file
func derivePkgName(registry, repo string) string {
	if registry != dockerhub {
		return fmt.Sprintf("%s/%s", registry, repo)
	}
	return repo
}

func determineDeps(o runtime.Object) ([]v1beta1.Dependency, error) {
	pkg, ok := xpkg.TryConvertToPkg(o, &xpmetav1.Provider{}, &xpmetav1.Configuration{})
	if !ok {
		return nil, errors.New(errFailedToConvertMetaToPackage)
	}

	out := make([]v1beta1.Dependency, len(pkg.GetDependencies()))
	for i, d := range pkg.GetDependencies() {
		out[i] = convertToV1beta1(d)
	}

	return out, nil
}

func convertToV1beta1(in xpmetav1.Dependency) v1beta1.Dependency {
	betaD := v1beta1.Dependency{
		Constraints: in.Version,
	}
	if in.Provider != nil && in.Configuration == nil {
		betaD.Package = *in.Provider
		betaD.Type = v1beta1.ProviderPackageType
	}

	if in.Configuration != nil && in.Provider == nil {
		betaD.Package = *in.Configuration
		betaD.Type = v1beta1.ConfigurationPackageType
	}

	return betaD
}

func validatorsFromV1Beta1CRD(c *v1beta1ext.CustomResourceDefinition, acc map[schema.GroupVersionKind]*validate.SchemaValidator) error {

	internal := &apiextensions.CustomResourceDefinition{}
	if err := v1beta1ext.Convert_v1beta1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(c, internal, nil); err != nil {
		return err
	}

	if internal.Spec.Validation != nil {
		sv, _, err := validation.NewSchemaValidator(internal.Spec.Validation)
		if err != nil {
			return err
		}
		for _, v := range internal.Spec.Versions {
			acc[gvk(internal.Spec.Group, v.Name, internal.Spec.Names.Kind)] = sv
		}
		return nil
	}
	for _, v := range internal.Spec.Versions {
		sv, _, err := validation.NewSchemaValidator(v.Schema)
		if err != nil {
			return err
		}
		acc[gvk(internal.Spec.Group, v.Name, internal.Spec.Names.Kind)] = sv
	}

	return nil
}

func validatorsFromV1CRD(c *v1ext.CustomResourceDefinition, acc map[schema.GroupVersionKind]*validate.SchemaValidator) error {

	for _, v := range c.Spec.Versions {
		sv, _, err := newV1SchemaValidator(*v.Schema.OpenAPIV3Schema)
		if err != nil {
			return err
		}
		acc[gvk(c.Spec.Group, v.Name, c.Spec.Names.Kind)] = sv
	}

	return nil
}

func validatorsFromV1Beta1XRD(x *xpv1beta1ext.CompositeResourceDefinition, acc map[schema.GroupVersionKind]*validate.SchemaValidator) error {
	for _, v := range x.Spec.Versions {
		var props v1ext.JSONSchemaProps
		if err := json.Unmarshal(v.Schema.OpenAPIV3Schema.Raw, &props); err != nil {
			return err
		}

		sv, _, err := newV1SchemaValidator(props)
		if err != nil {
			return err
		}

		acc[gvk(x.Spec.Group, v.Name, x.Spec.Names.Kind)] = sv
	}
	return nil
}

func validatorsFromV1XRD(x *xpv1ext.CompositeResourceDefinition, acc map[schema.GroupVersionKind]*validate.SchemaValidator) error {
	for _, ver := range x.Spec.Versions {
		var props v1ext.JSONSchemaProps
		if err := json.Unmarshal(ver.Schema.OpenAPIV3Schema.Raw, &props); err != nil {
			return err
		}

		sv, _, err := newV1SchemaValidator(props)
		if err != nil {
			return err
		}

		acc[gvk(x.Spec.Group, ver.Name, x.Spec.Names.Kind)] = sv
	}
	return nil
}

// newSchemaValidator creates an openapi schema validator for the given JSONSchemaProps validation.
func newV1SchemaValidator(schema v1ext.JSONSchemaProps) (*validate.SchemaValidator, *spec.Schema, error) { //nolint:unparam
	// Convert CRD schema to openapi schema
	openapiSchema := &spec.Schema{}
	out := new(apiextensions.JSONSchemaProps)
	if err := v1ext.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&schema, out, nil); err != nil {
		return nil, nil, err
	}
	if err := validation.ConvertJSONSchemaPropsWithPostProcess(out, openapiSchema, validation.StripUnsupportedFormatsPostProcess); err != nil {
		return nil, nil, err
	}
	return validate.NewSchemaValidator(openapiSchema, nil, "", strfmt.Default), openapiSchema, nil
}

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	}
}

func skipDigest(digest *string) parser.FilterFn {
	return func(path string, info os.FileInfo) (bool, error) {
		match := false
		base := filepath.Base(path)
		if strings.HasPrefix(base, digestPrefix) {
			*digest = base
			match = true
		}
		return match, nil
	}
}
